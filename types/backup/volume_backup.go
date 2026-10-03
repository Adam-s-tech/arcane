package backup

import "strings"

// Volume backup run statuses and triggers.
const (
	VolumeBackupStatusRunning   = "running"
	VolumeBackupStatusSucceeded = "succeeded"
	VolumeBackupStatusFailed    = "failed"

	VolumeBackupTriggerManual    = "manual"
	VolumeBackupTriggerScheduled = "scheduled"
	VolumeBackupTriggerSafety    = "safety"
)

// ManagementTypeForPolicy reports which configuration orchestrated a backup created under policyID.
func ManagementTypeForPolicy(policyID string) ManagementType {
	if strings.HasPrefix(policyID, SystemVolumePolicyPrefix) {
		return ManagementTypeSystem
	}
	return ManagementTypeVolume
}
