package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func newOwnedTestCluster(name, namespace string, instances int32) *redisv1.RedisCluster {
	cluster := newTestCluster(name, namespace, instances)
	cluster.UID = types.UID("11111111-2222-3333-4444-555555555555")
	return cluster
}

func getPod(t *testing.T, r *ClusterReconciler, namespace, name string) *corev1.Pod {
	t.Helper()
	var pod corev1.Pod
	require.NoError(t, r.Get(context.Background(), types.NamespacedName{
		Name: name, Namespace: namespace,
	}, &pod))
	return &pod
}

func TestCreatePod_SetsControllerReferenceAndSafeToEvict(t *testing.T) {
	cluster := newOwnedTestCluster("test", "default", 1)
	r, _ := newReconciler(cluster)

	require.NoError(t, r.createPod(context.Background(), cluster, "test-0", 0, redisv1.LabelRolePrimary))

	pod := getPod(t, r, "default", "test-0")
	assert.True(t, metav1.IsControlledBy(pod, cluster), "pod should be controlled by the RedisCluster")
	assert.Equal(t, "true", pod.Annotations[safeToEvictAnnotation])
}

func TestCreatePod_AdoptsExistingOwnerlessPod(t *testing.T) {
	cluster := newOwnedTestCluster("test", "default", 1)
	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels:    podLabels(cluster.Name, "test-0", redisv1.LabelRolePrimary),
		},
	}
	r, _ := newReconciler(cluster, existing)

	require.NoError(t, r.createPod(context.Background(), cluster, "test-0", 0, redisv1.LabelRolePrimary))

	pod := getPod(t, r, "default", "test-0")
	assert.True(t, metav1.IsControlledBy(pod, cluster), "pre-existing pod should be adopted")
	assert.Equal(t, "true", pod.Annotations[safeToEvictAnnotation])
}

func TestCreatePod_LeavesForeignOwnerReferenceAlone(t *testing.T) {
	cluster := newOwnedTestCluster("test", "default", 1)
	other := newOwnedTestCluster("other", "default", 1)
	other.UID = types.UID("99999999-9999-9999-9999-999999999999")
	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels:    podLabels(cluster.Name, "test-0", redisv1.LabelRolePrimary),
		},
	}
	require.NoError(t, controllerutil.SetControllerReference(other, existing, testScheme()))
	r, _ := newReconciler(cluster, existing)

	require.NoError(t, r.createPod(context.Background(), cluster, "test-0", 0, redisv1.LabelRolePrimary))

	pod := getPod(t, r, "default", "test-0")
	require.Len(t, pod.OwnerReferences, 1)
	assert.Equal(t, other.UID, pod.OwnerReferences[0].UID)
}

func TestCreateSentinelPod_SetsControllerReferenceAndSafeToEvict(t *testing.T) {
	cluster := newOwnedTestCluster("test", "default", 1)
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	r, _ := newReconciler(cluster)

	podName := sentinelPodNameForIndex(cluster.Name, 0)
	require.NoError(t, r.createSentinelPod(context.Background(), cluster, podName))

	pod := getPod(t, r, "default", podName)
	assert.True(t, metav1.IsControlledBy(pod, cluster), "sentinel pod should be controlled by the RedisCluster")
	assert.Equal(t, "true", pod.Annotations[safeToEvictAnnotation])
}

func TestCreateSentinelPod_AdoptsExistingOwnerlessPod(t *testing.T) {
	cluster := newOwnedTestCluster("test", "default", 1)
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	podName := sentinelPodNameForIndex(cluster.Name, 0)
	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: "default",
			Labels:    podLabels(cluster.Name, podName, redisv1.LabelRoleSentinel),
		},
	}
	r, _ := newReconciler(cluster, existing)

	require.NoError(t, r.createSentinelPod(context.Background(), cluster, podName))

	pod := getPod(t, r, "default", podName)
	assert.True(t, metav1.IsControlledBy(pod, cluster), "pre-existing sentinel pod should be adopted")
	assert.Equal(t, "true", pod.Annotations[safeToEvictAnnotation])
}
