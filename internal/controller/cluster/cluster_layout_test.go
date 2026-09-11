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
			for shard := range layout.ownerOf {
				assert.False(t, layout.handingOver(shard), "no handover expected in this case")
			}
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

func TestPlanShardLayout_ScaleDownHandsDoomedOwnersToSurvivors(t *testing.T) {
	ranges := calculateClusterSlotRanges(3)

	t.Run("surviving replica inherits", func(t *testing.T) {
		cluster := newClusterModeCluster(3, 0)
		pods := []corev1.Pod{
			shardPod("test-0", "s0", "replica"), shardPod("test-1", "s1", "primary"),
			shardPod("test-2", "s2", "primary"), shardPod("test-3", "s0", "primary"),
			shardPod("test-4", "s1", "replica"), shardPod("test-5", "s2", "replica"),
		}
		statuses := map[string]redisv1.InstanceStatus{
			"test-0": replicaStatus("n0", "n3"), "test-3": ownerStatus("n3", ranges[0]),
			"test-1": ownerStatus("n1", ranges[1]), "test-4": replicaStatus("n4", "n1"),
			"test-2": ownerStatus("n2", ranges[2]), "test-5": replicaStatus("n5", "n2"),
		}
		layout := planShardLayout(cluster, pods, statuses)
		assert.Equal(t, "test-3", layout.ownerOf[0])
		assert.Equal(t, "test-0", layout.primaryOf[0])
		assert.True(t, layout.handingOver(0))
		assert.False(t, layout.handingOver(1))
		assert.Equal(t, map[int]string{0: "test-0", 1: "test-1", 2: "test-2"}, layout.primaryOf)
	})

	t.Run("legacy layout borrows a surviving replica when a whole shard is doomed", func(t *testing.T) {
		cluster := newClusterModeCluster(3, 0)
		pods := []corev1.Pod{
			shardPod("test-0", "s0", "primary"), shardPod("test-1", "s0", "replica"),
			shardPod("test-2", "s1", "primary"), shardPod("test-3", "s1", "replica"),
			shardPod("test-4", "s2", "primary"), shardPod("test-5", "s2", "replica"),
		}
		statuses := map[string]redisv1.InstanceStatus{
			"test-0": ownerStatus("n0", ranges[0]), "test-1": replicaStatus("n1", "n0"),
			"test-2": ownerStatus("n2", ranges[1]), "test-3": replicaStatus("n3", "n2"),
			"test-4": ownerStatus("n4", ranges[2]), "test-5": replicaStatus("n5", "n4"),
		}
		layout := planShardLayout(cluster, pods, statuses)
		assert.Equal(t, "test-4", layout.ownerOf[2])
		assert.Equal(t, "test-1", layout.primaryOf[2], "pod 1 is the only survivor that is not a primary")
		assert.Equal(t, 2, layout.shardOf["test-1"])
		assert.Equal(t, []string{"test-0"}, layout.members[0])
		assert.True(t, layout.handingOver(2))
	})
}

func TestPlanShardLayout_ReplicaRebalancing(t *testing.T) {
	ranges := calculateClusterSlotRanges(3)
	tests := []struct {
		name      string
		replicas  int32
		primary   []int
		follows   map[int]int
		wantMoves map[string]int
	}{
		{"legacy downscale", 1, []int{0, 3, 1}, map[int]int{2: 0, 4: 3, 5: 3}, map[string]int{"test-5": 2}},
		{"multiple deficient shards", 2, []int{0, 1, 2}, map[int]int{3: 0, 4: 0, 5: 0, 6: 0, 7: 0, 8: 0}, map[string]int{"test-8": 1, "test-7": 1, "test-6": 2, "test-5": 2}},
		{"all shards already replicated", 2, []int{0, 4, 8}, map[int]int{1: 0, 2: 0, 3: 8, 5: 4, 6: 4, 7: 4}, map[string]int{"test-7": 2}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := newClusterModeCluster(3, tt.replicas)
			statuses := map[string]redisv1.InstanceStatus{}
			for shard, index := range tt.primary {
				statuses[podNameForIndex("test", index)] = ownerStatus(podNameForIndex("node", index), ranges[shard])
			}
			for index, primary := range tt.follows {
				statuses[podNameForIndex("test", index)] = replicaStatus(podNameForIndex("node", index), podNameForIndex("node", primary))
			}
			layout := planShardLayout(cluster, nil, statuses)
			assert.Equal(t, tt.wantMoves, layout.replicaMoves)
			for name, target := range tt.wantMoves {
				assert.NotEqual(t, target, layout.shardOf[name], "status must still describe observed membership")
			}
		})
	}
}
