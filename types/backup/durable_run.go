package backup

import "github.com/getarcaneapp/arcane/types/v2/scheduler"

// DurableRunCommand freezes the input accepted by a backup domain.
type DurableRunCommand struct {
	UserID           string `msgpack:"userID"`
	EnvironmentID    string `msgpack:"environmentID"`
	Permission       string `msgpack:"permission"`
	RequestedWithKey string `msgpack:"requestedWithKey"`
	Kind             string `msgpack:"kind"`
	RunID            string `msgpack:"runID"`
	ActivityID       string `msgpack:"activityID"`
	Payload          []byte `msgpack:"payload"`
}

// DurableRunState records dispatch and effect evidence across host restarts.
type DurableRunState struct {
	Command DurableRunCommand         `msgpack:"command"`
	Started bool                      `msgpack:"started"`
	Status  scheduler.RunStatus       `msgpack:"status"`
	Targets []scheduler.TargetOutcome `msgpack:"targets"`
	Error   string                    `msgpack:"error"`
}
