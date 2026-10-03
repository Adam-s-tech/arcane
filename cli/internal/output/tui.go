package output

import (
	"context"
	"fmt"
	"io"
	"strings"
	"sync"
	"time"

	"charm.land/bubbles/v2/progress"
	"github.com/charmbracelet/x/ansi"
	"github.com/charmbracelet/x/term"
	"go.getarcane.app/sys/bytes"
)

var spinnerFrames = []string{"⠋", "⠙", "⠹", "⠸", "⠼", "⠴", "⠦", "⠧", "⠇", "⠏"}

var (
	trackersMu     sync.Mutex
	activeTrackers = make(map[*tracker]struct{})
	terminalMu     sync.Mutex
)

const (
	stepPending = iota
	stepRunning
	stepSucceeded
	stepFailed
)

type trackerStep struct {
	key, label     string
	status         int
	current, total int64
	bytes          bool
}

type tracker struct {
	mu                            sync.Mutex
	out                           io.Writer
	live                          bool
	steps                         []trackerStep
	current, drawn, frame, paused int
	stopped                       bool
	stop, done                    chan struct{}
	once                          sync.Once
	resumeOuter                   func()
}

// StartTracker renders a checklist only for interactive text output.
func StartTracker(ctx context.Context, out io.Writer, enabled bool, labels ...string) *tracker {
	if out == nil {
		out = io.Discard
	}
	t := &tracker{out: out, live: enabled && IsTerminal(out) && IsTerminal(Stdout()), current: -1}
	for _, label := range labels {
		t.steps = append(t.steps, trackerStep{label: label, status: stepPending})
	}
	if !t.live || ctx.Err() != nil {
		t.live = false
		return t
	}
	t.stop, t.done = make(chan struct{}), make(chan struct{})
	t.resumeOuter = SuspendProgress()
	trackersMu.Lock()
	activeTrackers[t] = struct{}{}
	trackersMu.Unlock()
	go t.runInternal(ctx)
	return t
}

func (t *tracker) runInternal(ctx context.Context) {
	defer close(t.done)
	defer func() {
		t.mu.Lock()
		defer t.mu.Unlock()
		t.stopped = true
		t.clearInternal()
	}()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.stop:
			return
		case <-ticker.C:
			t.mu.Lock()
			t.frame++
			if t.paused == 0 {
				t.drawInternal()
			}
			t.mu.Unlock()
		}
	}
}

// NextStep completes the active step and starts the next pending step.
func (t *tracker) NextStep() {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.current >= 0 && t.current < len(t.steps) {
		t.steps[t.current].status = stepSucceeded
	}
	t.current++
	if t.current < len(t.steps) {
		t.steps[t.current].status = stepRunning
	}
}

// SetProgress updates a distinct step, retaining interleaved layer totals.
func (t *tracker) SetProgress(key, label string, current, total int64, byteCount bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	index := -1
	if key == "" && t.current >= 0 && t.current < len(t.steps) {
		index = t.current
	} else {
		for i := range t.steps {
			if t.steps[i].key == key {
				index = i
				break
			}
		}
	}
	if index < 0 {
		t.steps = append(t.steps, trackerStep{key: key})
		index = len(t.steps) - 1
	}
	step := &t.steps[index]
	step.label, step.current, step.total, step.bytes = label, max(current, 0), max(total, 0), byteCount
	step.status = stepRunning
}

// Finish marks one tracked step complete or failed.
func (t *tracker) Finish(key string, err error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for i := range t.steps {
		if (key == "" && i == t.current) || (key != "" && t.steps[i].key == key) {
			t.steps[i].status = stepSucceeded
			if err != nil {
				t.steps[i].status = stepFailed
			}
		}
	}
}

// Stop clears loading and waits for the renderer. It is safe to repeat.
func (t *tracker) Stop() {
	if t == nil || !t.live {
		return
	}
	t.once.Do(func() {
		close(t.stop)
		<-t.done
		trackersMu.Lock()
		delete(activeTrackers, t)
		trackersMu.Unlock()
		t.resumeOuter()
	})
}

// SuspendProgress pauses every active tracker until the returned function runs.
func SuspendProgress() func() {
	trackersMu.Lock()
	trackers := make([]*tracker, 0, len(activeTrackers))
	for t := range activeTrackers {
		trackers = append(trackers, t)
	}
	trackersMu.Unlock()
	for _, t := range trackers {
		t.mu.Lock()
		t.paused++
		t.clearInternal()
		t.mu.Unlock()
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			for _, t := range trackers {
				t.mu.Lock()
				t.paused = max(t.paused-1, 0)
				t.mu.Unlock()
			}
		})
	}
}

func (t *tracker) clearInternal() {
	if t.drawn == 0 {
		return
	}
	terminalMu.Lock()
	defer terminalMu.Unlock()
	_, _ = io.WriteString(t.out, strings.Repeat("\x1b[A\r\x1b[2K", t.drawn))
	t.drawn = 0
}

func (t *tracker) drawInternal() {
	width := 80
	height := 24
	if fd, ok := t.out.(interface{ Fd() uintptr }); ok {
		if w, h, err := term.GetSize(fd.Fd()); err == nil {
			width, height = max(w, 1), max(h, 2)
		}
	}
	var frame strings.Builder
	if t.drawn > 0 {
		_, _ = fmt.Fprintf(&frame, "\x1b[%dA", t.drawn)
	}
	start := max(len(t.steps)-max(height-2, 1), 0)
	for _, step := range t.steps[start:] {
		frame.WriteString("\r\x1b[2K")
		frame.WriteString(ansi.Truncate(t.renderStepInternal(step, width), width, "…"))
		frame.WriteByte('\n')
	}
	// Erase rows left over after a terminal resize.
	count := len(t.steps) - start
	for i := count; i < t.drawn; i++ {
		frame.WriteString("\r\x1b[2K\n")
	}
	if t.drawn > count {
		_, _ = fmt.Fprintf(&frame, "\x1b[%dA", t.drawn-count)
	}
	t.drawn = count
	terminalMu.Lock()
	defer terminalMu.Unlock()
	_, _ = io.WriteString(t.out, frame.String())
}

func (t *tracker) renderStepInternal(step trackerStep, width int) string {
	icon, style := "○", statusMutedStyle
	switch step.status {
	case stepRunning:
		icon, style = spinnerFrames[t.frame%len(spinnerFrames)], infoStyle
		if step.total > 0 {
			current := min(step.current, step.total)
			count := fmt.Sprintf("%d/%d", current, step.total)
			if step.bytes {
				count = Bytes(current) + "/" + Bytes(step.total)
			}
			barWidth := min(24, max(width-visibleWidthInternal(step.label)-len(count)-7, 1))
			bar := progress.New(progress.WithWidth(barWidth), progress.WithColors(arcanePurple))
			if !colorEnabledInternal() {
				bar.FullColor = nil
				bar.EmptyColor = nil
				bar.PercentageStyle = tablePlainCell.UnsetPadding()
			}
			line := fmt.Sprintf("  %s %s %s", renderForInternal(t.out, infoStyle, step.label), bar.ViewAs(float64(current)/float64(step.total)), count)
			if !colorEnabledInternal() {
				return ansi.Strip(line)
			}
			return line
		}
	case stepSucceeded:
		icon, style = "✓", successStyle
	case stepFailed:
		icon, style = "✗", warnStyle.Foreground(statusOffline)
	}
	return "  " + renderForInternal(t.out, style, icon) + " " + renderForInternal(t.out, valueStyle, step.label)
}

// Bytes renders a signed byte count in human-readable form.
func Bytes(value int64) string { return bytes.Capacity(uint64(max(value, 0))).String() }

// UnsignedBytes renders an unsigned byte count in human-readable form.
func UnsignedBytes(value uint64) string { return bytes.Capacity(value).String() }
