package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ScheduledBackupPhase represents the phase of a scheduled backup.
type ScheduledBackupPhase string

const (
	ScheduledBackupPhaseActive    ScheduledBackupPhase = "Active"
	ScheduledBackupPhaseSuspended ScheduledBackupPhase = "Suspended"
)

// RedisScheduledBackupSpec defines the desired state of a RedisScheduledBackup.
type RedisScheduledBackupSpec struct {
	// Schedule is a cron expression defining when to run backups.
	// +kubebuilder:validation:MinLength=1
	Schedule string `json:"schedule"`

	// ClusterName is the name of the RedisCluster to back up.
	ClusterName string `json:"clusterName"`

	// Target specifies which pod to run the backup on.
	// +kubebuilder:default="prefer-replica"
	Target BackupTarget `json:"target,omitempty"`

	// Method specifies the backup method.
	// +kubebuilder:default="rdb"
	Method BackupMethod `json:"method,omitempty"`

	// Destination defines where to store the backup.
	// +optional
	Destination *BackupDestination `json:"destination,omitempty"`

	// Suspend stops future backups from being scheduled when true.
	// +optional
	Suspend *bool `json:"suspend,omitempty"`

	// SuccessfulBackupsHistoryLimit is the number of successful backups to retain.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +optional
	SuccessfulBackupsHistoryLimit *int32 `json:"successfulBackupsHistoryLimit,omitempty"`

	// FailedBackupsHistoryLimit is the number of failed backups to retain.
	// +kubebuilder:default=3
	// +kubebuilder:validation:Minimum=0
	// +optional
	FailedBackupsHistoryLimit *int32 `json:"failedBackupsHistoryLimit,omitempty"`
}

// RedisScheduledBackupStatus defines the observed state of a RedisScheduledBackup.
type RedisScheduledBackupStatus struct {
	// Phase is the current phase.
	Phase ScheduledBackupPhase `json:"phase,omitempty"`

	// LastScheduleTime is the last time a backup was triggered.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// NextScheduleTime is when the next backup will be triggered.
	// +optional
	NextScheduleTime *metav1.Time `json:"nextScheduleTime,omitempty"`

	// LastBackupName is the name of the most recently created RedisBackup.
	// +optional
	LastBackupName string `json:"lastBackupName,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rsb
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Last Backup",type=date,JSONPath=`.status.lastScheduleTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RedisScheduledBackup is the Schema for the redisscheduledbackups API.
type RedisScheduledBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RedisScheduledBackupSpec   `json:"spec,omitempty"`
	Status RedisScheduledBackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RedisScheduledBackupList contains a list of RedisScheduledBackup.
type RedisScheduledBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RedisScheduledBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RedisScheduledBackup{}, &RedisScheduledBackupList{})
}
