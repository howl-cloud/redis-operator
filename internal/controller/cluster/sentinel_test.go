package cluster

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestReconcileSentinelMaster_TriesNextSentinelOnFailure(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	cluster.Status.CurrentPrimary = "test-0"

	dataPrimary := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels:    podLabels("test", "test-0", redisv1.LabelRolePrimary),
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.1"},
	}
	dataReplica := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-1",
			Namespace: "default",
			Labels:    podLabels("test", "test-1", redisv1.LabelRoleReplica),
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.2"},
	}
	sentinel0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sentinel-0",
			Namespace: "default",
			Labels:    podLabels("test", "test-sentinel-0", redisv1.LabelRoleSentinel),
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.10"},
	}
	sentinel1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sentinel-1",
			Namespace: "default",
			Labels:    podLabels("test", "test-sentinel-1", redisv1.LabelRoleSentinel),
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.11"},
	}
	leaderSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      leaderServiceName(cluster.Name),
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-0",
			},
		},
	}

	r, c := newReconciler(cluster, dataPrimary, dataReplica, sentinel0, sentinel1, leaderSvc)
	oldQuery := querySentinelMasterFn
	querySentinelMasterFn = func(_ context.Context, addr, _ string) (string, int, error) {
		if strings.HasPrefix(addr, "10.0.0.10:") {
			return "", 0, errors.New("sentinel down")
		}
		return "10.0.0.2", 6379, nil
	}
	t.Cleanup(func() { querySentinelMasterFn = oldQuery })

	err := r.reconcileSentinelMaster(context.Background(), cluster)
	require.NoError(t, err)

	var updatedCluster redisv1.RedisCluster
	err = c.Get(context.Background(), types.NamespacedName{Name: "test", Namespace: "default"}, &updatedCluster)
	require.NoError(t, err)
	assert.Equal(t, "test-1", updatedCluster.Status.CurrentPrimary)

	var updatedLeader corev1.Service
	err = c.Get(context.Background(), types.NamespacedName{Name: leaderServiceName(cluster.Name), Namespace: "default"}, &updatedLeader)
	require.NoError(t, err)
	assert.Equal(t, "test-1", updatedLeader.Spec.Selector[redisv1.LabelInstance])

	var updatedPrimary corev1.Pod
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-1", Namespace: "default"}, &updatedPrimary)
	require.NoError(t, err)
	assert.Equal(t, redisv1.LabelRolePrimary, updatedPrimary.Labels[redisv1.LabelRole])

	var updatedFormer corev1.Pod
	err = c.Get(context.Background(), types.NamespacedName{Name: "test-0", Namespace: "default"}, &updatedFormer)
	require.NoError(t, err)
	assert.Equal(t, redisv1.LabelRoleReplica, updatedFormer.Labels[redisv1.LabelRole])
}

func TestReconcileSentinelMaster_AllSentinelsFail(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	cluster.Status.CurrentPrimary = "test-0"

	sentinel0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-sentinel-0",
			Namespace: "default",
			Labels:    podLabels("test", "test-sentinel-0", redisv1.LabelRoleSentinel),
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.10"},
	}
	r, _ := newReconciler(cluster, sentinel0)
	oldQuery := querySentinelMasterFn
	querySentinelMasterFn = func(_ context.Context, _, _ string) (string, int, error) {
		return "", 0, errors.New("unreachable")
	}
	t.Cleanup(func() { querySentinelMasterFn = oldQuery })

	err := r.reconcileSentinelMaster(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "failed querying sentinel master")
}
