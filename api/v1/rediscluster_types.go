package v1

import (
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// ClusterMode defines the Redis operating mode.
// +kubebuilder:validation:Enum=standalone;sentinel;cluster
type ClusterMode string

const (
	ClusterModeStandalone ClusterMode = "standalone"
	ClusterModeSentinel   ClusterMode = "sentinel"
	ClusterModeCluster    ClusterMode = "cluster"
)

// ClusterPhase represents the human-readable phase of a RedisCluster.
type ClusterPhase string

const (
	ClusterPhaseCreating    ClusterPhase = "Creating"
	ClusterPhaseHealthy     ClusterPhase = "Healthy"
	ClusterPhaseDegraded    ClusterPhase = "Degraded"
	ClusterPhaseFailingOver ClusterPhase = "FailingOver"
	ClusterPhaseScaling     ClusterPhase = "Scaling"
	ClusterPhaseUpdating    ClusterPhase = "Updating"
	ClusterPhaseDeleting     ClusterPhase = "Deleting"
	ClusterPhaseHibernating  ClusterPhase = "Hibernating"
)

// Condition types for RedisCluster.
const (
	ConditionReady               = "Ready"
	ConditionPrimaryAvailable    = "PrimaryAvailable"
	ConditionReplicationHealthy  = "ReplicationHealthy"
	ConditionLastBackupSucceeded = "LastBackupSucceeded"
	ConditionHibernated          = "Hibernated"
)

// Fencing annotation key. The annotation value is a JSON list of fenced pod names.
const (
	FencingAnnotationKey = "redis.io/fencedInstances"
)

// Hibernation annotation key. Set to "on" or "true" to hibernate the cluster.
const (
	AnnotationHibernation = "redis.io/hibernation"
)

// Labels used by the operator.
const (
	LabelCluster     = "redis.io/cluster"
	LabelInstance    = "redis.io/instance"
	LabelRole        = "redis.io/role"
	LabelRolePrimary = "primary"
	LabelRoleReplica = "replica"
)

// RedisClusterSpec defines the desired state of a Redis replication cluster.
type RedisClusterSpec struct {
	// Instances is the total number of Redis pods (1 primary + N-1 replicas).
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:default=1
	Instances int32 `json:"instances"`

	// Mode defines the Redis operating mode.
	// +kubebuilder:default=standalone
	Mode ClusterMode `json:"mode,omitempty"`

	// ImageName is the Redis container image.
	// +kubebuilder:default="redis:7.2"
	ImageName string `json:"imageName,omitempty"`

	// Storage defines the PVC template for /data volumes.
	Storage StorageSpec `json:"storage"`

	// Redis contains redis.conf configuration parameters.
	// +optional
	Redis map[string]string `json:"redis,omitempty"`

	// Resources defines CPU/memory requests and limits for Redis containers.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// MinSyncReplicas is the minimum number of synchronous replicas.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MinSyncReplicas int32 `json:"minSyncReplicas,omitempty"`

	// MaxSyncReplicas is the maximum number of synchronous replicas.
	// +kubebuilder:validation:Minimum=0
	// +optional
	MaxSyncReplicas int32 `json:"maxSyncReplicas,omitempty"`

	// EnablePodDisruptionBudget controls whether a PDB is created.
	// +kubebuilder:default=true
	EnablePodDisruptionBudget *bool `json:"enablePodDisruptionBudget,omitempty"`

	// AuthSecret references a Secret containing the Redis password in key "password".
	// If not set, the operator auto-generates one.
	// +optional
	AuthSecret *LocalObjectReference `json:"authSecret,omitempty"`

	// ACLConfigSecret references a Secret containing ACL rules in key "acl".
	// +optional
	ACLConfigSecret *LocalObjectReference `json:"aclConfigSecret,omitempty"`

	// TLSSecret references a Secret containing tls.crt and tls.key.
	// +optional
	TLSSecret *LocalObjectReference `json:"tlsSecret,omitempty"`

	// CASecret references a Secret containing ca.crt for TLS client verification.
	// +optional
	CASecret *LocalObjectReference `json:"caSecret,omitempty"`

	// BackupCredentialsSecret references a Secret containing object storage credentials.
	// +optional
	BackupCredentialsSecret *LocalObjectReference `json:"backupCredentialsSecret,omitempty"`

	// NodeSelector constrains pods to nodes with matching labels.
	// +optional
	NodeSelector map[string]string `json:"nodeSelector,omitempty"`

	// Affinity defines pod affinity/anti-affinity scheduling rules.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// Tolerations allow scheduling onto tainted nodes.
	// +optional
	Tolerations []corev1.Toleration `json:"tolerations,omitempty"`

	// TopologySpreadConstraints control how pods are spread across topology domains.
	// +optional
	TopologySpreadConstraints []corev1.TopologySpreadConstraint `json:"topologySpreadConstraints,omitempty"`

	// Bootstrap defines how to initialize the cluster from a backup.
	// +optional
	Bootstrap *BootstrapSpec `json:"bootstrap,omitempty"`
}

// StorageSpec defines PVC storage for Redis data.
type StorageSpec struct {
	// Size is the requested storage size.
	// +kubebuilder:default="1Gi"
	Size resource.Quantity `json:"size"`

	// StorageClassName is the name of the StorageClass.
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// LocalObjectReference is a reference to a Secret in the same namespace.
type LocalObjectReference struct {
	// Name of the referent.
	Name string `json:"name"`
}

// BootstrapSpec defines how to bootstrap the cluster from a backup.
type BootstrapSpec struct {
	// BackupName references a RedisBackup to restore from.
	BackupName string `json:"backupName"`
}

// InstanceStatus holds live status reported by a single instance manager.
type InstanceStatus struct {
	// Role is "master" or "slave" as reported by Redis INFO.
	Role string `json:"role,omitempty"`

	// Connected indicates whether the instance manager can reach Redis.
	Connected bool `json:"connected,omitempty"`

	// ReplicationOffset is the current replication offset.
	ReplicationOffset int64 `json:"replicationOffset,omitempty"`

	// ReplicaLagBytes is the replication lag in bytes (replicas only).
	ReplicaLagBytes int64 `json:"replicaLagBytes,omitempty"`

	// ConnectedReplicas is the number of connected replicas (primary only).
	ConnectedReplicas int32 `json:"connectedReplicas,omitempty"`

	// MasterLinkStatus is "up" or "down" (replicas only).
	MasterLinkStatus string `json:"masterLinkStatus,omitempty"`

	// LastSeenAt is when this status was last reported.
	// +optional
	LastSeenAt *metav1.Time `json:"lastSeenAt,omitempty"`
}

// RedisClusterStatus defines the observed state of a RedisCluster.
type RedisClusterStatus struct {
	// Phase is the human-readable cluster phase.
	Phase ClusterPhase `json:"phase,omitempty"`

	// CurrentPrimary is the pod name of the current primary instance.
	CurrentPrimary string `json:"currentPrimary,omitempty"`

	// ReadyInstances is the count of pods passing readiness probes.
	ReadyInstances int32 `json:"readyInstances,omitempty"`

	// Instances is the total number of managed pods.
	Instances int32 `json:"instances,omitempty"`

	// InstancesStatus is a per-pod status map keyed by pod name.
	// Using a map (not slice) to avoid strategic-merge-patch ordering issues.
	// +optional
	InstancesStatus map[string]InstanceStatus `json:"instancesStatus,omitempty"`

	// HealthyPVC is the count of healthy PVCs.
	HealthyPVC int32 `json:"healthyPVC,omitempty"`

	// DanglingPVC lists PVC names that are not attached to any pod.
	// +optional
	DanglingPVC []string `json:"danglingPVC,omitempty"`

	// SecretsResourceVersion maps secret names to their ResourceVersion.
	// Used to detect secret rotation and trigger reconciliation.
	// +optional
	SecretsResourceVersion map[string]string `json:"secretsResourceVersion,omitempty"`

	// Conditions represent the latest available observations of the cluster state.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=rc
// +kubebuilder:printcolumn:name="Instances",type=integer,JSONPath=`.spec.instances`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.readyInstances`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Primary",type=string,JSONPath=`.status.currentPrimary`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// RedisCluster is the Schema for the redisclusters API.
type RedisCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   RedisClusterSpec   `json:"spec,omitempty"`
	Status RedisClusterStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// RedisClusterList contains a list of RedisCluster.
type RedisClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []RedisCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&RedisCluster{}, &RedisClusterList{})
}
