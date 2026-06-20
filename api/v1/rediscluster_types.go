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

// StorageType defines how Redis data volumes are backed.
// +kubebuilder:validation:Enum=pvc;emptyDir
type StorageType string

const (
	// StorageTypePVC backs /data with a PersistentVolumeClaim (durable across pod recreation).
	StorageTypePVC StorageType = "pvc"
	// StorageTypeEmptyDir backs /data with pod-local emptyDir storage (data is lost on pod recreation).
	StorageTypeEmptyDir StorageType = "emptyDir"
)

// PrimaryUpdateStrategy defines how the primary is updated during rolling updates.
// +kubebuilder:validation:Enum=unsupervised;supervised
type PrimaryUpdateStrategy string

const (
	PrimaryUpdateStrategyUnsupervised PrimaryUpdateStrategy = "unsupervised"
	PrimaryUpdateStrategySupervised   PrimaryUpdateStrategy = "supervised"
)

// ClusterPhase represents the human-readable phase of a RedisCluster.
type ClusterPhase string

const (
	ClusterPhaseCreating       ClusterPhase = "Creating"
	ClusterPhaseHealthy        ClusterPhase = "Healthy"
	ClusterPhaseDegraded       ClusterPhase = "Degraded"
	ClusterPhaseReplicating    ClusterPhase = "Replicating"
	ClusterPhaseFailingOver    ClusterPhase = "FailingOver"
	ClusterPhaseScaling        ClusterPhase = "Scaling"
	ClusterPhaseUpdating       ClusterPhase = "Updating"
	ClusterPhaseWaitingForUser ClusterPhase = "WaitingForUser"
	ClusterPhaseDeleting       ClusterPhase = "Deleting"
	ClusterPhaseHibernating    ClusterPhase = "Hibernating"
)

// Condition types for RedisCluster.
const (
	ConditionReady                 = "Ready"
	ConditionPrimaryAvailable      = "PrimaryAvailable"
	ConditionReplicationHealthy    = "ReplicationHealthy"
	ConditionReplicaMode           = "ReplicaMode"
	ConditionPrimaryUpdateWaiting  = "PrimaryUpdateWaiting"
	ConditionLastBackupSucceeded   = "LastBackupSucceeded"
	ConditionHibernated            = "Hibernated"
	ConditionMaintenanceInProgress = "MaintenanceInProgress"
	ConditionPVCResizeInProgress   = "PVCResizeInProgress"
)

// Fencing annotation key. The annotation value is a JSON list of fenced pod names.
const (
	FencingAnnotationKey = "redis.io/fencedInstances"
)

// Hibernation annotation key. Set to "on" or "true" to hibernate the cluster.
const (
	AnnotationHibernation          = "redis.io/hibernation"
	AnnotationApprovePrimaryUpdate = "redis.io/approve-primary-update"
)

// Labels used by the operator.
const (
	LabelCluster          = "redis.io/cluster"
	LabelInstance         = "redis.io/instance"
	LabelRole             = "redis.io/role"
	LabelShard            = "redis.io/shard"
	LabelShardRole        = "redis.io/shard-role"
	LabelRolePrimary      = "primary"
	LabelRoleReplica      = "replica"
	LabelRoleSentinel     = "sentinel"
	LabelWorkload         = "redis.io/workload"
	LabelWorkloadData     = "data"
	LabelWorkloadSentinel = "sentinel"
)

// Sentinel defaults for sentinel mode.
const (
	SentinelPort      = 26379
	SentinelInstances = 3
	SentinelQuorum    = 2
)

// RedisClusterSpec defines the desired state of a Redis replication cluster.
type RedisClusterSpec struct {
	// Instances is the total number of Redis pods (1 primary + N-1 replicas).
	// This field is for standalone and sentinel modes only.
	// Cluster mode derives pod count from shards and replicasPerShard.
	// +kubebuilder:validation:Minimum=0
	// +optional
	Instances int32 `json:"instances,omitempty"`

	// Mode defines the Redis operating mode.
	// +kubebuilder:default=standalone
	Mode ClusterMode `json:"mode,omitempty"`

	// Shards is the number of hash-slot shards in cluster mode.
	// +kubebuilder:validation:Minimum=3
	// +optional
	Shards int32 `json:"shards,omitempty"`

	// ReplicasPerShard is the number of replicas per shard in cluster mode.
	// +kubebuilder:validation:Minimum=0
	// +optional
	ReplicasPerShard int32 `json:"replicasPerShard,omitempty"`

	// PrimaryUpdateStrategy controls whether primary replacement runs automatically
	// after replicas are updated (unsupervised), or waits for operator approval (supervised).
	// +kubebuilder:validation:Enum=unsupervised;supervised
	// +kubebuilder:default=unsupervised
	// +optional
	PrimaryUpdateStrategy PrimaryUpdateStrategy `json:"primaryUpdateStrategy,omitempty"`

	// ImageName is the Redis container image.
	// +kubebuilder:default="redis:7.2"
	ImageName string `json:"imageName,omitempty"`

	// Storage defines how /data volumes are backed (PVC or ephemeral emptyDir).
	Storage StorageSpec `json:"storage"`

	// Redis contains redis.conf configuration parameters.
	// +optional
	Redis map[string]string `json:"redis,omitempty"`

	// Resources defines CPU/memory requests and limits for Redis containers.
	// +optional
	Resources corev1.ResourceRequirements `json:"resources,omitempty"`

	// Memory configures Redis maxmemory and the eviction policy. Use this instead
	// of setting "maxmemory"/"maxmemory-policy" directly in spec.redis so the
	// operator can keep them consistent with the container memory limit.
	// +optional
	Memory *MemorySpec `json:"memory,omitempty"`

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

	// PrimaryIsolation configures runtime split-brain prevention for primary pods.
	// +optional
	PrimaryIsolation *PrimaryIsolationSpec `json:"primaryIsolation,omitempty"`

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

	// NodeMaintenanceWindow controls planned node maintenance behavior.
	// +optional
	NodeMaintenanceWindow *NodeMaintenanceWindow `json:"nodeMaintenanceWindow,omitempty"`

	// ReplicaMode configures a full-cluster external replication topology for DR.
	// +optional
	ReplicaMode *ReplicaModeSpec `json:"replicaMode,omitempty"`

	// ConnectionSecret, when set, makes the operator publish and maintain a Secret
	// containing ready-to-use connection details for this cluster (host, port,
	// url, leader/replica endpoints, and mode-specific discovery values). The
	// generated Secret embeds the password and a full redis:// URL, so it is
	// credential-bearing and lives under the same RBAC as the auth Secret. Leave
	// unset to skip publishing it.
	// +optional
	ConnectionSecret *ConnectionSecretSpec `json:"connectionSecret,omitempty"`
}

// ConnectionSecretSpec configures the operator-published connection Secret.
type ConnectionSecretSpec struct {
	// Name of the Secret to create and maintain in the cluster's namespace. The
	// operator owns this Secret: it is updated on auth rotation or service
	// changes and garbage-collected when the RedisCluster is deleted. If a Secret
	// with this name already exists and is not owned by this RedisCluster, the
	// operator refuses to overwrite it.
	// +kubebuilder:validation:MaxLength=253
	Name string `json:"name"`
}

// StorageSpec defines storage for Redis data.
type StorageSpec struct {
	// Type selects how /data is backed: "pvc" for a PersistentVolumeClaim (durable),
	// or "emptyDir" for pod-local ephemeral storage (data is lost when a pod is recreated).
	// StorageClassName, resize, and PVC lifecycle handling apply only to "pvc".
	// +kubebuilder:default=pvc
	// +optional
	Type StorageType `json:"type,omitempty"`

	// Size is the requested storage size. For "pvc" it is the PVC request;
	// for "emptyDir" it is the volume's sizeLimit (the pod is evicted if exceeded).
	// +kubebuilder:default="1Gi"
	Size resource.Quantity `json:"size"`

	// StorageClassName is the name of the StorageClass. Ignored for "emptyDir".
	// +optional
	StorageClassName *string `json:"storageClassName,omitempty"`
}

// IsEphemeral reports whether data is backed by pod-local ephemeral storage.
func (s StorageSpec) IsEphemeral() bool {
	return s.Type == StorageTypeEmptyDir
}

// MaxMemoryPolicy is the Redis eviction policy applied when maxmemory is reached.
// +kubebuilder:validation:Enum=noeviction;allkeys-lru;allkeys-lfu;volatile-lru;volatile-lfu;allkeys-random;volatile-random;volatile-ttl
type MaxMemoryPolicy string

const (
	// MaxMemoryPolicyNoEviction rejects writes once maxmemory is reached. This is
	// the Redis default and the operator default: it turns container OOM-kills into
	// predictable write errors without silently evicting data.
	MaxMemoryPolicyNoEviction MaxMemoryPolicy = "noeviction"
)

// MemorySpec configures Redis memory limits and eviction behavior. It is the
// first-class alternative to setting maxmemory/maxmemory-policy in spec.redis.
type MemorySpec struct {
	// MaxMemory sets an explicit Redis maxmemory (e.g. "256Mi", "2Gi"). It is
	// rendered as a byte count. Mutually exclusive with MaxMemoryPercent.
	// +optional
	MaxMemory *resource.Quantity `json:"maxMemory,omitempty"`

	// MaxMemoryPercent derives maxmemory as a percent of the container memory limit
	// (spec.resources.limits.memory). It requires that limit to be set and is
	// mutually exclusive with MaxMemory. Leave headroom (e.g. 75) for replication
	// buffers, copy-on-write during persistence, and fragmentation.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	// +optional
	MaxMemoryPercent *int32 `json:"maxMemoryPercent,omitempty"`

	// MaxMemoryPolicy sets the eviction policy (maxmemory-policy). Defaults to
	// noeviction, which rejects writes at the limit instead of evicting keys.
	// +optional
	MaxMemoryPolicy MaxMemoryPolicy `json:"maxMemoryPolicy,omitempty"`
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

// NodeMaintenanceWindow defines planned node maintenance behavior.
type NodeMaintenanceWindow struct {
	// InProgress indicates planned node maintenance is currently in progress.
	InProgress bool `json:"inProgress"`

	// ReusePVC controls whether PVCs are reused during maintenance.
	// +kubebuilder:default=true
	// +optional
	ReusePVC *bool `json:"reusePVC,omitempty"`
}

// ReplicaModeSpec defines external replication behavior for DR clusters.
type ReplicaModeSpec struct {
	// Enabled toggles external replication mode for all data pods.
	// +optional
	Enabled bool `json:"enabled,omitempty"`

	// Source identifies the external Redis primary to replicate from.
	// +optional
	Source *ReplicaSourceSpec `json:"source,omitempty"`

	// Promote requests promotion of the local designated leader to standalone primary.
	// +optional
	Promote bool `json:"promote,omitempty"`
}

// ReplicaSourceSpec identifies an external Redis source.
type ReplicaSourceSpec struct {
	// ClusterName is a human-readable source cluster identifier.
	// +optional
	ClusterName string `json:"clusterName,omitempty"`

	// Host is the external Redis endpoint.
	Host string `json:"host"`

	// Port is the external Redis port.
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=65535
	// +kubebuilder:default=6379
	// +optional
	Port int32 `json:"port,omitempty"`

	// AuthSecretName references a Secret with key "password" for upstream auth.
	// +optional
	AuthSecretName string `json:"authSecretName,omitempty"`
}

// PrimaryIsolationSpec defines runtime primary-isolation detection settings.
type PrimaryIsolationSpec struct {
	// Enabled controls runtime primary-isolation checks in /healthz.
	// +kubebuilder:default=true
	Enabled *bool `json:"enabled,omitempty"`

	// APIServerTimeout is the timeout for Kubernetes API reachability checks.
	// +kubebuilder:default="5s"
	// +optional
	APIServerTimeout *metav1.Duration `json:"apiServerTimeout,omitempty"`

	// PeerTimeout is the timeout for peer instance-manager reachability checks.
	// +kubebuilder:default="5s"
	// +optional
	PeerTimeout *metav1.Duration `json:"peerTimeout,omitempty"`
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

	// NodeID is the Redis cluster node ID.
	NodeID string `json:"nodeID,omitempty"`

	// SlotsServed lists slot ranges served by this node in cluster mode.
	// +optional
	SlotsServed []SlotRange `json:"slotsServed,omitempty"`

	// ClusterState is the Redis cluster state reported by this node.
	ClusterState string `json:"clusterState,omitempty"`

	// CurrentEpoch is the current Redis cluster configuration epoch.
	CurrentEpoch int64 `json:"currentEpoch,omitempty"`

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

	// SentinelReadyInstances is the count of sentinel pods passing readiness probes.
	SentinelReadyInstances int32 `json:"sentinelReadyInstances,omitempty"`

	// Instances is the total number of managed pods.
	Instances int32 `json:"instances,omitempty"`

	// InstancesStatus is a per-pod status map keyed by pod name.
	// Using a map (not slice) to avoid strategic-merge-patch ordering issues.
	// +optional
	InstancesStatus map[string]InstanceStatus `json:"instancesStatus,omitempty"`

	// ClusterState mirrors CLUSTER INFO cluster_state in cluster mode.
	ClusterState string `json:"clusterState,omitempty"`

	// SlotsAssigned mirrors CLUSTER INFO cluster_slots_assigned in cluster mode.
	SlotsAssigned int32 `json:"slotsAssigned,omitempty"`

	// BootstrapCompleted indicates the one-time bootstrap workflow has finished.
	BootstrapCompleted bool `json:"bootstrapCompleted,omitempty"`

	// Shards is a per-shard status map keyed by shard name.
	// +optional
	Shards map[string]ShardStatus `json:"shards,omitempty"`

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

// SlotRange describes an inclusive Redis hash slot range.
type SlotRange struct {
	Start int32 `json:"start"`
	End   int32 `json:"end"`
}

// ShardStatus captures per-shard cluster topology and slot ownership.
type ShardStatus struct {
	PrimaryPod string `json:"primaryPod,omitempty"`

	// +optional
	ReplicaPods []string `json:"replicaPods,omitempty"`

	// +optional
	SlotRanges []SlotRange `json:"slotRanges,omitempty"`

	PrimaryNodeID string `json:"primaryNodeID,omitempty"`
	Epoch         int64  `json:"epoch,omitempty"`
}

// DesiredDataInstances returns the desired number of Redis data pods for the spec.
func (s RedisClusterSpec) DesiredDataInstances() int32 {
	if s.Mode != ClusterModeCluster {
		return s.Instances
	}
	shards := s.Shards
	if shards < 3 {
		shards = 3
	}
	replicasPerShard := s.ReplicasPerShard
	if replicasPerShard < 0 {
		replicasPerShard = 0
	}
	return shards * (1 + replicasPerShard)
}

// MemoryLimitBytes returns the container memory limit in bytes, or 0 if unset.
func (s RedisClusterSpec) MemoryLimitBytes() int64 {
	if s.Resources.Limits == nil {
		return 0
	}
	q, ok := s.Resources.Limits[corev1.ResourceMemory]
	if !ok {
		return 0
	}
	return q.Value()
}

// ResolveMaxMemoryBytes computes the effective Redis maxmemory in bytes from
// spec.memory. It returns (bytes, true) when maxmemory should be configured, or
// (0, false) when it is not configured (or cannot be resolved). A percent is
// resolved against the container memory limit in spec.resources.limits.memory.
func (s RedisClusterSpec) ResolveMaxMemoryBytes() (int64, bool) {
	if s.Memory == nil {
		return 0, false
	}
	if s.Memory.MaxMemory != nil {
		if v := s.Memory.MaxMemory.Value(); v > 0 {
			return v, true
		}
		return 0, false
	}
	if s.Memory.MaxMemoryPercent != nil {
		pct := *s.Memory.MaxMemoryPercent
		limit := s.MemoryLimitBytes()
		if pct <= 0 || limit <= 0 {
			return 0, false
		}
		if bytes := limit * int64(pct) / 100; bytes > 0 {
			return bytes, true
		}
	}
	return 0, false
}

// EffectiveMaxMemoryPolicy returns the configured eviction policy, or "" if unset.
func (s RedisClusterSpec) EffectiveMaxMemoryPolicy() string {
	if s.Memory == nil {
		return ""
	}
	return string(s.Memory.MaxMemoryPolicy)
}
