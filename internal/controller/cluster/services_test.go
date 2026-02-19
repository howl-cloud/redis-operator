package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestEnsureService_Creates(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	selector := map[string]string{
		redisv1.LabelCluster: "test",
		redisv1.LabelRole:    redisv1.LabelRolePrimary,
	}
	err := r.ensureService(ctx, cluster, "test-leader", selector)
	require.NoError(t, err)

	var svc corev1.Service
	err = c.Get(ctx, types.NamespacedName{Name: "test-leader", Namespace: "default"}, &svc)
	require.NoError(t, err)

	assert.Equal(t, "test", svc.Labels[redisv1.LabelCluster])
	assert.Equal(t, selector, svc.Spec.Selector)
	require.Len(t, svc.Spec.Ports, 1)
	assert.Equal(t, "redis", svc.Spec.Ports[0].Name)
	assert.Equal(t, int32(6379), svc.Spec.Ports[0].Port)
}

func TestEnsureService_AlreadyExists(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	r, _ := newReconciler(cluster)
	ctx := context.Background()

	selector := map[string]string{redisv1.LabelCluster: "test"}

	// Call twice -- second should be a no-op.
	require.NoError(t, r.ensureService(ctx, cluster, "test-any", selector))
	require.NoError(t, r.ensureService(ctx, cluster, "test-any", selector))
}

func TestUpdateLeaderServiceSelector_Updates(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-1"

	// Pre-create the leader service pointing to test-0.
	leaderSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-leader",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-0",
			},
		},
	}

	r, c := newReconciler(cluster, leaderSvc)
	ctx := context.Background()

	err := r.updateLeaderServiceSelector(ctx, cluster)
	require.NoError(t, err)

	var updated corev1.Service
	err = c.Get(ctx, types.NamespacedName{Name: "test-leader", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, "test-1", updated.Spec.Selector[redisv1.LabelInstance])
}

func TestUpdateLeaderServiceSelector_AlreadyCurrent(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"

	leaderSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-leader",
			Namespace: "default",
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-0",
			},
		},
	}

	r, _ := newReconciler(cluster, leaderSvc)
	ctx := context.Background()

	// Should be a no-op (selector already correct).
	err := r.updateLeaderServiceSelector(ctx, cluster)
	require.NoError(t, err)
}
