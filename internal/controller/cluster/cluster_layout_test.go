package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func newClusterModeCluster(shards, replicasPerShard int32) *redisv1.RedisCluster {
	cluster := newTestCluster("test", "default", 0)
	cluster.Spec.Mode = redisv1.ClusterModeCluster
	cluster.Spec.Shards = shards
	cluster.Spec.ReplicasPerShard = replicasPerShard
	return cluster
}

func shardPod(name, shard, shardRole string) corev1.Pod {
	labels := map[string]string{redisv1.LabelCluster: "test"}
	if shard != "" {
		labels[redisv1.LabelShard] = shard
		labels[redisv1.LabelShardRole] = shardRole
	}
	return corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default", Labels: labels}}
}

func ownerStatus(nodeID string, slots redisv1.SlotRange) redisv1.InstanceStatus {
	return redisv1.InstanceStatus{Role: "master", Connected: true, NodeID: nodeID, SlotsServed: []redisv1.SlotRange{slots}}
}

func emptyPrimaryStatus(nodeID string) redisv1.InstanceStatus {
	return redisv1.InstanceStatus{Role: "master", Connected: true, NodeID: nodeID}
}

func replicaStatus(nodeID, primaryNodeID string) redisv1.InstanceStatus {
	return redisv1.InstanceStatus{Role: "slave", Connected: true, NodeID: nodeID, PrimaryNodeID: primaryNodeID}
}

func TestPlanShardLayout(t *testing.T) {
	ranges := calculateClusterSlotRanges(3)

	tests := []struct {
		name          string
		cluster       *redisv1.RedisCluster
		pods          []corev1.Pod
		statuses      map[string]redisv1.InstanceStatus
		wantShardOf   map[string]int
		wantPrimaryOf map[int]string
	}{
		{
			name:    "fresh cluster fills primaries first then balances replicas",
			cluster: newClusterModeCluster(3, 1),
			wantShardOf: map[string]int{
				"test-0": 0, "test-1": 1, "test-2": 2,
				"test-3": 0, "test-4": 1, "test-5": 2,
			},
			wantPrimaryOf: map[int]string{0: "test-0", 1: "test-1", 2: "test-2"},
		},
		{
			name:    "raising replicasPerShard keeps slot owners as primaries",
			cluster: newClusterModeCluster(3, 1),
			pods: []corev1.Pod{
				shardPod("test-0", "s0", "primary"),
				shardPod("test-1", "s1", "primary"),
				shardPod("test-2", "s2", "primary"),
			},
			statuses: map[string]redisv1.InstanceStatus{
				"test-0": ownerStatus("n0", ranges[0]),
				"test-1": ownerStatus("n1", ranges[1]),
				"test-2": ownerStatus("n2", ranges[2]),
			},
			wantShardOf: map[string]int{
				"test-0": 0, "test-1": 1, "test-2": 2,
				"test-3": 0, "test-4": 1, "test-5": 2,
			},
			wantPrimaryOf: map[int]string{0: "test-0", 1: "test-1", 2: "test-2"},
		},
		{
			name:    "pods mislabelled by the ordinal formula are re-placed as empty replicas",
			cluster: newClusterModeCluster(3, 1),
			pods: []corev1.Pod{
				shardPod("test-0", "s0", "primary"),
				shardPod("test-1", "s1", "primary"),
				shardPod("test-2", "s2", "primary"),
				shardPod("test-3", "s1", "replica"),
				shardPod("test-4", "s2", "primary"),
				shardPod("test-5", "s2", "replica"),
			},
			statuses: map[string]redisv1.InstanceStatus{
				"test-0": ownerStatus("n0", ranges[0]),
				"test-1": ownerStatus("n1", ranges[1]),
				"test-2": ownerStatus("n2", ranges[2]),
				"test-3": emptyPrimaryStatus("n3"),
				"test-4": emptyPrimaryStatus("n4"),
				"test-5": emptyPrimaryStatus("n5"),
			},
			wantShardOf: map[string]int{
				"test-0": 0, "test-1": 1, "test-2": 2,
				"test-3": 0, "test-4": 1, "test-5": 2,
			},
			wantPrimaryOf: map[int]string{0: "test-0", 1: "test-1", 2: "test-2"},
		},
		{
			name:    "legacy contiguous layout is preserved",
			cluster: newClusterModeCluster(3, 1),
			pods: []corev1.Pod{
				shardPod("test-0", "s0", "primary"), shardPod("test-1", "s0", "replica"),
				shardPod("test-2", "s1", "primary"), shardPod("test-3", "s1", "replica"),
				shardPod("test-4", "s2", "primary"), shardPod("test-5", "s2", "replica"),
			},
			statuses: map[string]redisv1.InstanceStatus{
				"test-0": ownerStatus("n0", ranges[0]), "test-1": replicaStatus("n1", "n0"),
				"test-2": ownerStatus("n2", ranges[1]), "test-3": replicaStatus("n3", "n2"),
				"test-4": ownerStatus("n4", ranges[2]), "test-5": replicaStatus("n5", "n4"),
			},
			wantShardOf: map[string]int{
				"test-0": 0, "test-1": 0, "test-2": 1, "test-3": 1, "test-4": 2, "test-5": 2,
			},
			wantPrimaryOf: map[int]string{0: "test-0", 1: "test-2", 2: "test-4"},
		},
		{
			name:    "redis-level failover keeps the shard index with the new slot owner",
			cluster: newClusterModeCluster(3, 1),
			pods: []corev1.Pod{
				shardPod("test-0", "s0", "primary"), shardPod("test-1", "s0", "replica"),
				shardPod("test-2", "s1", "primary"), shardPod("test-3", "s1", "replica"),
				shardPod("test-4", "s2", "primary"), shardPod("test-5", "s2", "replica"),
			},
			statuses: map[string]redisv1.InstanceStatus{
				"test-0": replicaStatus("n0", "n1"), "test-1": ownerStatus("n1", ranges[0]),
				"test-2": ownerStatus("n2", ranges[1]), "test-3": replicaStatus("n3", "n2"),
				"test-4": ownerStatus("n4", ranges[2]), "test-5": replicaStatus("n5", "n4"),
			},
			wantShardOf: map[string]int{
				"test-0": 0, "test-1": 0, "test-2": 1, "test-3": 1, "test-4": 2, "test-5": 2,
			},
			wantPrimaryOf: map[int]string{0: "test-1", 1: "test-2", 2: "test-4"},
		},
		{
			name:    "unreachable pod keeps its label",
			cluster: newClusterModeCluster(3, 1),
			pods: []corev1.Pod{
				shardPod("test-0", "s0", "primary"), shardPod("test-1", "s1", "primary"),
				shardPod("test-2", "s2", "primary"), shardPod("test-3", "s0", "replica"),
				shardPod("test-4", "s1", "replica"), shardPod("test-5", "s2", "replica"),
			},
			statuses: map[string]redisv1.InstanceStatus{
				"test-0": ownerStatus("n0", ranges[0]),
				"test-1": ownerStatus("n1", ranges[1]),
				"test-2": ownerStatus("n2", ranges[2]),
				"test-3": replicaStatus("n3", "n0"),
				"test-4": {Connected: false},
				"test-5": replicaStatus("n5", "n2"),
			},
			wantShardOf: map[string]int{
				"test-0": 0, "test-1": 1, "test-2": 2, "test-3": 0, "test-4": 1, "test-5": 2,
			},
			wantPrimaryOf: map[int]string{0: "test-0", 1: "test-1", 2: "test-2"},
		},
		{
			name:    "scaling shards up gives new pods the missing primaries",
			cluster: newClusterModeCluster(5, 0),
			pods: []corev1.Pod{
				shardPod("test-0", "s0", "primary"),
				shardPod("test-1", "s1", "primary"),
				shardPod("test-2", "s2", "primary"),
			},
			statuses: map[string]redisv1.InstanceStatus{
				"test-0": ownerStatus("n0", ranges[0]),
				"test-1": ownerStatus("n1", ranges[1]),
				"test-2": ownerStatus("n2", ranges[2]),
				"test-3": emptyPrimaryStatus("n3"),
				"test-4": emptyPrimaryStatus("n4"),
			},
			// Owners keep the shard whose new range they overlap most; the new
			// pods take the gaps, so the reshard moves the least data.
			wantShardOf:   map[string]int{"test-0": 0, "test-1": 2, "test-2": 4, "test-3": 1, "test-4": 3},
			wantPrimaryOf: map[int]string{0: "test-0", 1: "test-3", 2: "test-1", 3: "test-4", 4: "test-2"},
		},
		{
			name:    "owner labels are ignored in favour of the slots they serve",
			cluster: newClusterModeCluster(3, 0),
			pods: []corev1.Pod{
				shardPod("test-0", "s2", "primary"),
				shardPod("test-1", "s2", "replica"),
				shardPod("test-2", "s0", "primary"),
			},
			statuses: map[string]redisv1.InstanceStatus{
				"test-0": ownerStatus("n0", ranges[0]),
				"test-1": ownerStatus("n1", ranges[1]),
				"test-2": ownerStatus("n2", ranges[2]),
			},
			wantShardOf:   map[string]int{"test-0": 0, "test-1": 1, "test-2": 2},
			wantPrimaryOf: map[int]string{0: "test-0", 1: "test-1", 2: "test-2"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			layout := planShardLayout(tt.cluster, tt.pods, tt.statuses)
			assert.Equal(t, tt.wantShardOf, layout.shardOf)
			assert.Equal(t, tt.wantPrimaryOf, layout.primaryOf)
		})
	}
}

func TestShardLayoutLabels(t *testing.T) {
	layout := planShardLayout(newClusterModeCluster(3, 1), nil, nil)

	assert.Equal(t, map[string]string{
		redisv1.LabelShard:     "s1",
		redisv1.LabelShardRole: redisv1.LabelRolePrimary,
	}, layout.labels("test-1"))
	assert.Equal(t, map[string]string{
		redisv1.LabelShard:     "s1",
		redisv1.LabelShardRole: redisv1.LabelRoleReplica,
	}, layout.labels("test-4"))
	assert.Nil(t, layout.labels("test-9"))
}
