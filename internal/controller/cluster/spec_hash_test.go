package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestComputeSpecHash_StableForSameInput(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("128Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("500m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
	}
	cluster.Spec.Redis = map[string]string{
		"appendfsync": "everysec",
		"maxmemory":   "256mb",
	}
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "auth-secret"}
	cluster.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls-secret"}

	r, _ := newReconciler(cluster)

	hash1 := r.computeSpecHash(cluster)
	hash2 := r.computeSpecHash(cluster)

	assert.Equal(t, hash1, hash2)
	assert.NotEmpty(t, hash1)
}

func TestComputeSpecHash_ChangesWhenTrackedFieldsChange(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Resources = corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU: resource.MustParse("100m"),
		},
	}
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "auth-secret"}
	cluster.Spec.Redis = map[string]string{
		"maxmemory-policy": "allkeys-lru",
	}

	baseReconciler, _ := newReconciler(cluster)
	baseHash := baseReconciler.computeSpecHash(cluster)

	testCases := []struct {
		name             string
		mutateCluster    func(*redisv1.RedisCluster)
		mutateReconciler func(*ClusterReconciler)
	}{
		{
			name: "redis image changes hash",
			mutateCluster: func(c *redisv1.RedisCluster) {
				c.Spec.ImageName = "redis:7.2.1"
			},
		},
		{
			name: "resources changes hash",
			mutateCluster: func(c *redisv1.RedisCluster) {
				c.Spec.Resources.Requests[corev1.ResourceMemory] = resource.MustParse("192Mi")
			},
		},
		{
			name: "operator image changes hash",
			mutateReconciler: func(r *ClusterReconciler) {
				r.OperatorImage = "example.com/redis-operator:v2"
			},
		},
		{
			name: "secret references change hash",
			mutateCluster: func(c *redisv1.RedisCluster) {
				c.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "rotated-auth-secret"}
			},
		},
		{
			name: "redis config changes hash",
			mutateCluster: func(c *redisv1.RedisCluster) {
				c.Spec.Redis["maxmemory-policy"] = "volatile-lru"
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			clusterCopy := cluster.DeepCopy()
			reconcilerCopy := *baseReconciler

			if tc.mutateCluster != nil {
				tc.mutateCluster(clusterCopy)
			}
			if tc.mutateReconciler != nil {
				tc.mutateReconciler(&reconcilerCopy)
			}

			mutatedHash := reconcilerCopy.computeSpecHash(clusterCopy)
			assert.NotEqual(t, baseHash, mutatedHash)
		})
	}
}

func TestCreatePod_SetsSpecHashLabelOnNewPods(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      pvcNameForIndex("test", 0),
			Namespace: "default",
		},
	}

	r, c := newReconciler(cluster, pvc)
	ctx := context.Background()

	err := r.createPod(ctx, cluster, "test-0", 0, redisv1.LabelRolePrimary)
	require.NoError(t, err)

	var pod corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-0", Namespace: "default"}, &pod)
	require.NoError(t, err)
	assert.Equal(t, r.computeSpecHash(cluster), pod.Annotations[specHashAnnotation])
}

func TestCreatePod_DoesNotOverwriteSpecHashOnExistingPods(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	existingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "test",
			},
			Annotations: map[string]string{
				specHashAnnotation: "old-hash",
			},
		},
	}

	r, c := newReconciler(cluster, existingPod)
	ctx := context.Background()

	err := r.createPod(ctx, cluster, "test-0", 0, redisv1.LabelRolePrimary)
	require.NoError(t, err)

	var updated corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-0", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, "old-hash", updated.Annotations[specHashAnnotation])
	assert.Equal(t, redisv1.LabelRolePrimary, updated.Labels[redisv1.LabelRole])
	assert.Equal(t, "test-0", updated.Labels[redisv1.LabelInstance])
}

func TestReconcile_RollingUpdateDeletesOutdatedReplica(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"

	hashReconciler, _ := newReconciler()
	desiredHash := hashReconciler.computeSpecHash(cluster)

	primaryPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-0",
				redisv1.LabelRole:     redisv1.LabelRolePrimary,
			},
			Annotations: map[string]string{
				specHashAnnotation: desiredHash,
			},
		},
	}
	outdatedReplica := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-1",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-1",
				redisv1.LabelRole:     redisv1.LabelRoleReplica,
			},
			Annotations: map[string]string{
				specHashAnnotation: "old-hash",
			},
		},
	}

	r, c := newReconciler(cluster, primaryPod, outdatedReplica)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.NoError(t, err)

	var deletedReplica corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-1", Namespace: "default"}, &deletedReplica)
	assert.Error(t, err)

	var primaryStillPresent corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-0", Namespace: "default"}, &primaryStillPresent)
	assert.NoError(t, err)
}
