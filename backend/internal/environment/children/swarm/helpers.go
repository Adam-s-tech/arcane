package swarm

import (
	"strings"
)

func buildAgentURLInternal(nodeID string) string {
	shortNodeID := nodeID
	if len(shortNodeID) > 12 {
		shortNodeID = shortNodeID[:12]
	}
	return "edge://swarm-node-" + shortNodeID
}

func buildAgentNameInternal(hostname, nodeID string) string {
	trimmedHostname := strings.TrimSpace(hostname)
	if trimmedHostname != "" {
		return trimmedHostname
	}
	if len(nodeID) > 12 {
		nodeID = nodeID[:12]
	}
	return "Swarm Node " + nodeID
}
