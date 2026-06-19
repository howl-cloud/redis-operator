package v1

import (
	"fmt"

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

// BackupArtifactType defines the backup artifact format stored in object storage.
// +kubebuilder:validation:Enum=rdb;aof-archive
type BackupArtifactType string

const (
	BackupArtifactTypeRDB        BackupArtifactType = "rdb"
	BackupArtifactTypeAOFArchive BackupArtifactType = "aof-archive"
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
// Exactly one backend (s3 or azure) must be set.
type BackupDestination struct {
	// S3 defines an S3-compatible storage destination.
	// +optional
	S3 *S3Destination `json:"s3,omitempty"`

	// Azure defines an Azure Blob Storage destination.
	// +optional
	Azure *AzureBlobDestination `json:"azure,omitempty"`
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

// Validate checks that exactly one backend is configured with its required fields.
func (d *BackupDestination) Validate() error {
	if d == nil {
		return fmt.Errorf("destination is required")
	}

	configured := 0
	if d.S3 != nil {
		configured++
	}
	if d.Azure != nil {
		configured++
	}
	switch {
	case configured == 0:
		return fmt.Errorf("destination requires exactly one of s3 or azure")
	case configured > 1:
		return fmt.Errorf("destination must set exactly one of s3 or azure")
	}

	if d.S3 != nil && d.S3.Bucket == "" {
		return fmt.Errorf("destination.s3.bucket is required")
	}
	if d.Azure != nil && d.Azure.Container == "" {
		return fmt.Errorf("destination.azure.container is required")
	}
	return nil
}

// AzureBlobDestination defines Azure Blob Storage parameters.
//
// Credentials are resolved from the cluster's backupCredentialsSecret and are
// auto-detected by key presence, in priority order:
//   - AZURE_STORAGE_CONNECTION_STRING (full connection string)
//   - AZURE_STORAGE_ACCOUNT + AZURE_STORAGE_KEY (shared key)
//   - AZURE_STORAGE_SAS_TOKEN (SAS, combined with accountName/endpoint)
type AzureBlobDestination struct {
	// Container is the Azure Blob container name.
	Container string `json:"container"`

	// Path is the prefix/path within the container.
	// +optional
	Path string `json:"path,omitempty"`

	// AccountName is the storage account name. Used to build the service URL
	// for shared-key and SAS auth. If empty, it falls back to AZURE_STORAGE_ACCOUNT
	// in the credentials secret. Ignored when a connection string is supplied.
	// +optional
	AccountName string `json:"accountName,omitempty"`

	// Endpoint overrides the blob service URL (for Azurite or sovereign clouds).
	// When unset, https://{accountName}.blob.core.windows.net is used.
	// +optional
	Endpoint string `json:"endpoint,omitempty"`
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

	// BackupPath is the object storage path/key where the backup is written.
	// +optional
	BackupPath string `json:"backupPath,omitempty"`

	// ArtifactType indicates the format of the backup artifact in object storage.
	// +optional
	ArtifactType BackupArtifactType `json:"artifactType,omitempty"`

	// ShardArtifacts contains per-shard backup metadata for cluster-mode backups.
	// +optional
	ShardArtifacts map[string]ShardBackupArtifact `json:"shardArtifacts,omitempty"`

	// Error contains the error message if the backup failed.
	// +optional
	Error string `json:"error,omitempty"`

	// Conditions represent the latest available observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// ShardBackupArtifact contains backup metadata for a single shard.
type ShardBackupArtifact struct {
	TargetPod string `json:"targetPod,omitempty"`

	BackupPath string `json:"backupPath,omitempty"`
	BackupSize int64  `json:"backupSize,omitempty"`

	ArtifactType BackupArtifactType `json:"artifactType,omitempty"`

	// +optional
	ChecksumSHA256 string `json:"checksumSHA256,omitempty"`
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
