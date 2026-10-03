package project

import "time"

// Project status values shared by stored projects and runtime projections.
const (
	StatusRunning          = "running"
	StatusStopped          = "stopped"
	StatusPartiallyRunning = "partially running"
	StatusUnknown          = "unknown"
	StatusDeploying        = "deploying"
	StatusStopping         = "stopping"
	StatusRestarting       = "restarting"
)

// Record is the stored state of a project that list projections read.
type Record struct {
	ID                 string
	Name               string
	DirName            *string
	Path               string
	Status             string
	StatusReason       *string
	ServiceCount       int
	RunningCount       int
	GitOpsManagedBy    *string
	ComposeProjectName *string
	BuildImageRefsJSON *string
	IsArchived         bool
	ArchivedAt         *time.Time
	CreatedAt          time.Time
	UpdatedAt          *time.Time
}

// TagAssignment is one stored project tag association.
type TagAssignment struct {
	ProjectID string
	Name      string
	Source    TagSource
	Color     TagColor
}

// ComposeIdentity is the compose project identity of a discovered directory.
type ComposeIdentity struct {
	ServiceCount        int
	ResolvedProjectName string
	ComposeProjectName  *string
	ExplicitProjectName bool
}
