package v1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// BackupPhase represents the phase of a backup.
type BackupPhase string

const (
	BackupPhasePending   BackupPhase = "Pending"
	BackupPhaseRunning   BackupPhase = "Running"
	BackupPhaseCompleted BackupPhase = "Completed"
	BackupPhaseFailed    BackupPhase = "Failed"
)

// BackupTarget defines where to run the backup.
// +kubebuilder:validation:Enum=primary;prefer-replica
type BackupTarget string

const (
	BackupTargetPrimary       BackupTarget = "primary"
	BackupTargetPreferReplica BackupTarget = "prefer-replica"
)

// BackupMethod defines the backup method.
// +kubebuilder:validation:Enum=rdb;aof
type BackupMethod string

const (
	BackupMethodRDB BackupMethod = "rdb"
	BackupMethodAOF BackupMethod = "aof"
)

// RedisBackupSpec defines the desired state of a RedisBackup.
type RedisBackupSpec struct {
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
}

// BackupDestination defines the object storage destination for backups.
type BackupDestination struct {
	// S3 defines an S3-compatible storage destination.
	// +optional
	S3 *S3Destination `json:"s3,omitempty"`
}

// S3Destination defines S3-compatible storage parameters.
type S3Destination struct {
	// Bucket is the S3 bucket name.
	Bucket string `json:"bucket"`

	// Path is the prefix/path within the bucket.
	// +optional
	Path string `json:"path,omitempty"`

	// Endpoint is the S3 endpoint URL (for non-AWS S3 compatible stores).
	// +optional
	Endpoint string `json:"endpoint,omitempty"`

	// Region is the AWS region.
	// +optional
	Region string `json:"region,omitempty"`
}

// RedisBackupStatus defines the observed state of a RedisBackup.
type RedisBackupStatus struct {
	// Phase is the current backup phase.
	Phase BackupPhase `json:"phase,omitempty"`

	// TargetPod is the pod that is running or ran the backup.
	TargetPod string `json:"targetPod,omitempty"`

	// StartedAt is when the backup started.
	// +optional
	StartedAt *metav1.Time `json:"startedAt,omitempty"`

	// CompletedAt is when the backup completed.
	// +optional
	CompletedAt *metav1.Time `json:"completedAt,omitempty"`

	// BackupSize is the size of the backup in bytes.
	// +optional
	BackupSize int64 `json:"backupSize,omitempty"`

	// Error contains the error message if the backup failed.
	// +optional
	Error string `json:"error,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rb
// +kubebuilder:printcolumn:name="Cluster",type=string,JSONPath=`.spec.clusterName`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Target",type=string,JSONPath=`.status.targetPod`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RedisBackup is the Schema for the redisbackups API.
type RedisBackup struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RedisBackupSpec   `json:"spec,omitempty"`
	Status RedisBackupStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RedisBackupList contains a list of RedisBackup.
type RedisBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RedisBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RedisBackup{}, &RedisBackupList{})
}
