package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// scaleDownFixture is a 6-pod cluster scaling to 3 where pod 3 owns shard s0
// after a failover and pod 0 replicates from it.
func scaleDownFixture(t *testing.T, heir redisv1.InstanceStatus) (*ClusterReconciler, *redisv1.RedisCluster, map[string]redisv1.InstanceStatus, *[]recordedPost) {
	t.Helper()
	cluster := newClusterModeCluster(3, 0)
	cluster.Status.ClusterState = "ok"
	cluster.Status.SlotsAssigned = 16384

	pods := []corev1.Pod{
		shardPod("test-0", "s0", "replica"), shardPod("test-1", "s1", "primary"),
		shardPod("test-2", "s2", "primary"), shardPod("test-3", "s0", "primary"),
		shardPod("test-4", "s1", "replica"), shardPod("test-5", "s2", "replica"),
	}
	objs := []client.Object{cluster}
	for i := range pods {
		pods[i].Status.PodIP = "127.0.0.1"
		objs = append(objs, &pods[i])
	}
	posts := fakeInstanceManagers(t, map[string]string{})

	ranges := calculateClusterSlotRanges(3)
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": heir, "test-3": ownerStatus("n3", ranges[0]),
		"test-1": ownerStatus("n1", ranges[1]), "test-4": replicaStatus("n4", "n1"),
		"test-2": ownerStatus("n2", ranges[2]), "test-5": replicaStatus("n5", "n2"),
	}
	r, _ := newReconciler(objs...)
	return r, cluster, statuses, posts
}

func endpoints(posts []recordedPost) []string {
	var out []string
	for _, post := range posts {
		out = append(out, post.endpoint)
	}
	return out
}

func TestReconcileClusterReshard_PromotesSurvivingReplicaOfDoomedOwner(t *testing.T) {
	heir := replicaStatus("n0", "n3")
	heir.MasterLinkStatus = "up"
	r, cluster, statuses, posts := scaleDownFixture(t, heir)

	ready, err := r.reconcileClusterReshard(context.Background(), cluster, statuses)
	require.NoError(t, err)
	assert.False(t, ready, "pod deletion must wait for the handover")
	assert.Empty(t, *posts, "promotion waits for the persisted fence to withdraw readiness")
	assert.Equal(t, "test-3/test-0", cluster.Annotations[redisv1.ClusterHandoverAnnotation])
	assert.Contains(t, r.getFencedPods(cluster), "test-3")
}

func TestReconcileClusterReshard_WaitsForHeirLinkBeforePromoting(t *testing.T) {
	heir := replicaStatus("n0", "n3")
	heir.MasterLinkStatus = "down"
	r, cluster, statuses, posts := scaleDownFixture(t, heir)

	ready, err := r.reconcileClusterReshard(context.Background(), cluster, statuses)
	require.NoError(t, err)
	assert.False(t, ready)
	assert.Empty(t, *posts)
}

func TestReconcileClusterReshard_AttachesHeirBeforePromoting(t *testing.T) {
	// Pod 0 is an empty master, not yet following the doomed owner.
	r, cluster, statuses, posts := scaleDownFixture(t, emptyPrimaryStatus("n0"))

	ready, err := r.reconcileClusterReshard(context.Background(), cluster, statuses)
	require.NoError(t, err)
	assert.False(t, ready)
	require.Equal(t, []string{"/v1/cluster/replicate"}, endpoints(*posts))
	assert.Equal(t, "n3", (*posts)[0].body["nodeID"])
}

func TestReconcileClusterReshard_NothingToDoWhenOwnersSurvive(t *testing.T) {
	cluster := newClusterModeCluster(3, 1)
	cluster.Status.ClusterState = "ok"
	cluster.Status.SlotsAssigned = 16384
	pods := []corev1.Pod{
		shardPod("test-0", "s0", "primary"), shardPod("test-1", "s1", "primary"),
		shardPod("test-2", "s2", "primary"), shardPod("test-3", "s0", "replica"),
		shardPod("test-4", "s1", "replica"), shardPod("test-5", "s2", "replica"),
	}
	objs := []client.Object{cluster}
	for i := range pods {
		pods[i].Status.PodIP = "127.0.0.1"
		objs = append(objs, &pods[i])
	}
	posts := fakeInstanceManagers(t, map[string]string{})
	ranges := calculateClusterSlotRanges(3)
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": ownerStatus("n0", ranges[0]), "test-3": replicaStatus("n3", "n0"),
		"test-1": ownerStatus("n1", ranges[1]), "test-4": replicaStatus("n4", "n1"),
		"test-2": ownerStatus("n2", ranges[2]), "test-5": replicaStatus("n5", "n2"),
	}
	r, _ := newReconciler(objs...)

	ready, err := r.reconcileClusterReshard(context.Background(), cluster, statuses)
	require.NoError(t, err)
	assert.True(t, ready)
	assert.Empty(t, *posts)
}
