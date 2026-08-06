package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
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

func TestCreatePod_SetsControllerReference(t *testing.T) {
	cluster := newOwnedTestCluster("test", "default", 1)
	r, _ := newReconciler(cluster)

	require.NoError(t, r.createPod(context.Background(), cluster, "test-0", 0, redisv1.LabelRolePrimary))

	pod := getPod(t, r, "default", "test-0")
	assert.True(t, metav1.IsControlledBy(pod, cluster), "pod should be controlled by the RedisCluster")
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
}

// A pod may carry owner references that are not the controller reference, for
// example one added by an external tool. Those do not make the pod managed, so
// it still needs adopting.
func TestCreatePod_AdoptsPodWithNonControllerOwnerReference(t *testing.T) {
	cluster := newOwnedTestCluster("test", "default", 1)
	existing := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels:    podLabels(cluster.Name, "test-0", redisv1.LabelRolePrimary),
		},
	}
	require.NoError(t, controllerutil.SetOwnerReference(newOwnedTestCluster("bystander", "default", 1), existing, testScheme()))
	require.Nil(t, metav1.GetControllerOf(existing), "precondition: pod has no controller reference")
	r, _ := newReconciler(cluster, existing)

	require.NoError(t, r.createPod(context.Background(), cluster, "test-0", 0, redisv1.LabelRolePrimary))

	pod := getPod(t, r, "default", "test-0")
	assert.True(t, metav1.IsControlledBy(pod, cluster), "pod with only non-controller owners should be adopted")
	assert.Len(t, pod.OwnerReferences, 2, "the pre-existing owner reference should be kept")
}

func TestCreatePod_LeavesForeignControllerAlone(t *testing.T) {
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

// Now that pods are evictable, they can disappear between reconciles. Nothing
// watches pods; recreation relies on the unconditional RequeueAfter
// requeueInterval and this create-on-NotFound path.
func TestCreatePod_RecreatesDeletedPod(t *testing.T) {
	ctx := context.Background()
	cluster := newOwnedTestCluster("test", "default", 1)
	r, _ := newReconciler(cluster)

	require.NoError(t, r.createPod(ctx, cluster, "test-0", 0, redisv1.LabelRolePrimary))
	pod := getPod(t, r, "default", "test-0")
	require.True(t, metav1.IsControlledBy(pod, cluster))

	require.NoError(t, r.Delete(ctx, pod))
	err := r.Get(ctx, types.NamespacedName{Name: "test-0", Namespace: "default"}, &corev1.Pod{})
	require.True(t, apierrors.IsNotFound(err), "precondition: pod is gone")

	require.NoError(t, r.createPod(ctx, cluster, "test-0", 0, redisv1.LabelRolePrimary))

	recreated := getPod(t, r, "default", "test-0")
	assert.True(t, metav1.IsControlledBy(recreated, cluster), "recreated pod should be controlled by the RedisCluster")
}

func TestCreateSentinelPod_SetsControllerReference(t *testing.T) {
	cluster := newOwnedTestCluster("test", "default", 1)
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	r, _ := newReconciler(cluster)

	podName := sentinelPodNameForIndex(cluster.Name, 0)
	require.NoError(t, r.createSentinelPod(context.Background(), cluster, podName))

	pod := getPod(t, r, "default", podName)
	assert.True(t, metav1.IsControlledBy(pod, cluster), "sentinel pod should be controlled by the RedisCluster")
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
}
