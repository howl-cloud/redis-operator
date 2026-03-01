package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestReconcileClusterBootstrap_DoesNotBlockWhenPodsAreNotCreatedYet(t *testing.T) {
	cluster := newTestCluster("test", "default", 0)
	cluster.Spec.Mode = redisv1.ClusterModeCluster
	cluster.Spec.Shards = 3
	cluster.Spec.ReplicasPerShard = 0

	r, _ := newReconciler(cluster)

	ready, err := r.reconcileClusterBootstrap(context.Background(), cluster, map[string]redisv1.InstanceStatus{})
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestReconcileClusterBootstrap_DoesNotBlockWhenPodStatusesAreMissing(t *testing.T) {
	cluster := newTestCluster("test", "default", 0)
	cluster.Spec.Mode = redisv1.ClusterModeCluster
	cluster.Spec.Shards = 3
	cluster.Spec.ReplicasPerShard = 0

	pod0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "test",
			},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.10"},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-1",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "test",
			},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.11"},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-2",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "test",
			},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.12"},
	}

	r, _ := newReconciler(cluster, pod0, pod1, pod2)

	ready, err := r.reconcileClusterBootstrap(context.Background(), cluster, map[string]redisv1.InstanceStatus{})
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestCalculateUncoveredSlotRanges_ReturnsMissingSegments(t *testing.T) {
	coverage := calculateSlotCoverage(map[string]redisv1.InstanceStatus{
		"node-a": {
			SlotsServed: []redisv1.SlotRange{
				{Start: 0, End: 100},
				{Start: 300, End: 350},
			},
		},
	})

	missing := calculateUncoveredSlotRanges(redisv1.SlotRange{Start: 0, End: 400}, &coverage)
	assert.Equal(t, []redisv1.SlotRange{
		{Start: 101, End: 299},
		{Start: 351, End: 400},
	}, missing)
}
