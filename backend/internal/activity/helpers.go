package activity

import (
	"cmp"
	"context"
	"sync"
	"time"

	"github.com/getarcaneapp/arcane/types/v2/activity"
	"github.com/samber/mo"
	"gorm.io/gorm"

	"github.com/getarcaneapp/arcane/backend/v2/internal/middleware"
	"github.com/getarcaneapp/arcane/backend/v2/internal/settings"
	"github.com/getarcaneapp/arcane/backend/v2/pkg/authz"
)

const (
	maxConcurrentActivitiesSettingKey = "maxConcurrentActivities"
	defaultMaxConcurrentActivities    = 5
	// slotWaitRecheckInterval bounds how long a queued waiter can miss a limit
	// increase made while no slots were being released.
	slotWaitRecheckInterval = 2 * time.Second
)

// activitySlotLimiter bounds how many queue-opted activities run concurrently
// per environment. The limit is read from settings on every acquisition
// attempt, while held slots are counted independently of it — so changing the
// limit mid-flight never miscounts running work: existing holders keep
// occupying capacity until they release, and waiters are re-evaluated against
// the new limit.
type activitySlotLimiter struct {
	settings *settings.SettingsService

	mu    sync.Mutex
	state map[string]*activityEnvironmentSlots
}

type activityEnvironmentSlots struct {
	held int
	// wake is closed (and replaced) on every release to broadcast to waiters;
	// each re-checks against the current limit, so a raised limit can admit
	// more than one of them.
	wake chan struct{}
}

func newActivitySlotLimiterInternal(localSettings *settings.SettingsService) *activitySlotLimiter {
	return &activitySlotLimiter{
		settings: localSettings,
		state:    map[string]*activityEnvironmentSlots{},
	}
}

func (l *activitySlotLimiter) limitInternal(ctx context.Context) int {
	return l.settings.GetIntSetting(ctx, maxConcurrentActivitiesSettingKey, defaultMaxConcurrentActivities)
}

func (l *activitySlotLimiter) stateForLockedInternal(environmentID string) *activityEnvironmentSlots {
	slots := l.state[environmentID]
	if slots == nil {
		slots = &activityEnvironmentSlots{wake: make(chan struct{})}
		l.state[environmentID] = slots
	}
	return slots
}

// tryAcquireInternal grabs a slot without blocking; None means the caller
// should queue and block via acquireInternal. Held slots are counted even
// while the limit is unlimited, so enabling a limit later still sees the work
// already running.
func (l *activitySlotLimiter) tryAcquireInternal(ctx context.Context, environmentID string) mo.Option[func()] {
	if l == nil || l.settings == nil {
		return mo.Some(func() {})
	}
	limit := l.limitInternal(ctx)

	l.mu.Lock()
	defer l.mu.Unlock()
	slots := l.stateForLockedInternal(environmentID)
	if limit > 0 && slots.held >= limit {
		return mo.None[func()]()
	}
	slots.held++
	return mo.Some(l.releaseOnceInternal(environmentID))
}

// acquireInternal blocks until a slot frees or ctx is cancelled. The limit is
// re-read on every attempt so setting changes apply to queued waiters.
func (l *activitySlotLimiter) acquireInternal(ctx context.Context, environmentID string) (func(), error) {
	if l == nil || l.settings == nil {
		return func() {}, nil
	}

	for {
		limit := l.limitInternal(ctx)
		l.mu.Lock()
		slots := l.stateForLockedInternal(environmentID)
		if limit <= 0 || slots.held < limit {
			slots.held++
			l.mu.Unlock()
			return l.releaseOnceInternal(environmentID), nil
		}
		wake := slots.wake
		l.mu.Unlock()

		timer := time.NewTimer(slotWaitRecheckInterval)
		select {
		case <-wake:
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, context.Cause(ctx)
		}
		timer.Stop()
	}
}

func (l *activitySlotLimiter) releaseOnceInternal(environmentID string) func() {
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			slots := l.stateForLockedInternal(environmentID)
			if slots.held > 0 {
				slots.held--
			}
			close(slots.wake)
			slots.wake = make(chan struct{})
			l.mu.Unlock()
		})
	}
}

// Job summaries live in their target environment's environment_id column.
func scopeJobActivityVisibilityInternal(ctx context.Context, query *gorm.DB, environmentID, permission string) *gorm.DB {
	permissions, present := middleware.PermissionsFromContext(ctx)
	if !present || permissions.Allows(permission, environmentID) {
		return query
	}
	return query.Where("type <> ?", activity.TypeJobRun)
}

func canReadJobActivityInternal(ctx context.Context, item activity.Activity) bool {
	if item.Type != activity.TypeJobRun {
		return true
	}
	metadataEnvironmentID, _ := item.Metadata["environmentId"].(string)
	environmentID := cmp.Or(item.EnvironmentID, metadataEnvironmentID)
	if environmentID == "" {
		return false
	}
	permissions, _ := middleware.PermissionsFromContext(ctx)
	return permissions.Allows(authz.PermActivitiesRead, environmentID)
}

func (h *ActivityHandler) canReadActivityStreamEventInternal(ctx context.Context, event activity.StreamEvent) bool {
	if event.Activity != nil {
		return canReadJobActivityInternal(ctx, *event.Activity)
	}
	if event.ActivityID == "" {
		return true
	}
	// Only job activities are scoped by target environment. Events that carry a
	// type hint for any other activity type need no database reread; job
	// metadata is mutable, so job events are still checked against the row.
	if event.ActivityType != "" && event.ActivityType != activity.TypeJobRun {
		return true
	}
	var model Activity
	// Message events carry no activity metadata. Check the owning row before output.
	if err := h.activityService.db.WithContext(ctx).Select("type", "environment_id", "metadata").Where("id = ? AND environment_id = ?", event.ActivityID, "0").First(&model).Error; err != nil {
		return false
	}
	return canReadJobActivityInternal(ctx, activity.Activity{Type: model.Type, EnvironmentID: model.EnvironmentID, Metadata: model.Metadata})
}
