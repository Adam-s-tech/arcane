package environment

// SwarmBindingCandidate is the binding-relevant state of an environment being attached to a node.
type SwarmBindingCandidate struct {
	Hidden              bool
	Enabled             bool
	ParentEnvironmentID *string
	SwarmNodeID         *string
}
