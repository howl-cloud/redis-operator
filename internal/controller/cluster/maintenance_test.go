package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestIsMaintenanceInProgress(t *testing.T) {
	tests := []struct {
		name     string
		window   *redisv1.NodeMaintenanceWindow
		expected bool
	}{
		{
			name:     "no maintenance window",
			window:   nil,
			expected: false,
		},
		{
			name: "maintenance window disabled",
			window: &redisv1.NodeMaintenanceWindow{
				InProgress: false,
			},
			expected: false,
		},
		{
			name: "maintenance window enabled",
			window: &redisv1.NodeMaintenanceWindow{
				InProgress: true,
			},
			expected: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := newTestCluster("test", "default", 3)
			cluster.Spec.NodeMaintenanceWindow = tt.window
			assert.Equal(t, tt.expected, isMaintenanceInProgress(cluster))
		})
	}
}

func TestMaintenanceReusePVC(t *testing.T) {
	tests := []struct {
		name     string
		window   *redisv1.NodeMaintenanceWindow
		expected bool
	}{
		{
			name:     "no maintenance window defaults true",
			window:   nil,
			expected: true,
		},
		{
			name: "nil reusePVC defaults true",
			window: &redisv1.NodeMaintenanceWindow{
				InProgress: true,
			},
			expected: true,
		},
		{
			name: "reusePVC true",
			window: &redisv1.NodeMaintenanceWindow{
				InProgress: true,
				ReusePVC:   boolPtr(true),
			},
			expected: true,
		},
		{
			name: "reusePVC false",
			window: &redisv1.NodeMaintenanceWindow{
				InProgress: true,
				ReusePVC:   boolPtr(false),
			},
			expected: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := newTestCluster("test", "default", 3)
			cluster.Spec.NodeMaintenanceWindow = tt.window
			assert.Equal(t, tt.expected, maintenanceReusePVC(cluster))
		})
	}
}

func TestReconcileMaintenance_StartsAndDeletesPDB(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Spec.NodeMaintenanceWindow = &redisv1.NodeMaintenanceWindow{
		InProgress: true,
		ReusePVC:   boolPtr(true),
	}

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pdb",
			Namespace: "default",
		},
	}

	r, c := newReconciler(cluster, pdb)
	ctx := context.Background()

	maintenance, err := r.reconcileMaintenance(ctx, cluster)
	require.NoError(t, err)
	assert.True(t, maintenance)

	var currentPDB policyv1.PodDisruptionBudget
	err = c.Get(ctx, types.NamespacedName{Name: "test-pdb", Namespace: "default"}, &currentPDB)
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))

	var updated redisv1.RedisCluster
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated))

	condition := meta.FindStatusCondition(updated.Status.Conditions, redisv1.ConditionMaintenanceInProgress)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionTrue, condition.Status)
	assert.Equal(t, "MaintenanceEnabled", condition.Reason)
}

func TestReconcileMaintenance_EndsAndClearsCondition(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Spec.NodeMaintenanceWindow = &redisv1.NodeMaintenanceWindow{
		InProgress: false,
		ReusePVC:   boolPtr(true),
	}
	cluster.Status.Conditions = []metav1.Condition{
		{
			Type:   redisv1.ConditionMaintenanceInProgress,
			Status: metav1.ConditionTrue,
			Reason: "MaintenanceEnabled",
		},
	}

	r, c := newReconciler(cluster)
	ctx := context.Background()

	maintenance, err := r.reconcileMaintenance(ctx, cluster)
	require.NoError(t, err)
	assert.False(t, maintenance)

	var updated redisv1.RedisCluster
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated))

	condition := meta.FindStatusCondition(updated.Status.Conditions, redisv1.ConditionMaintenanceInProgress)
	require.NotNil(t, condition)
	assert.Equal(t, metav1.ConditionFalse, condition.Status)
	assert.Equal(t, "MaintenanceDisabled", condition.Reason)
}

func TestReconcileMaintenance_AlreadyInProgressStillDeletesPDB(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Spec.NodeMaintenanceWindow = &redisv1.NodeMaintenanceWindow{
		InProgress: true,
		ReusePVC:   boolPtr(true),
	}
	cluster.Status.Conditions = []metav1.Condition{
		{
			Type:   redisv1.ConditionMaintenanceInProgress,
			Status: metav1.ConditionTrue,
			Reason: "MaintenanceEnabled",
		},
	}

	pdb := &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-pdb",
			Namespace: "default",
		},
	}

	r, c := newReconciler(cluster, pdb)
	ctx := context.Background()

	maintenance, err := r.reconcileMaintenance(ctx, cluster)
	require.NoError(t, err)
	assert.True(t, maintenance)

	var currentPDB policyv1.PodDisruptionBudget
	err = c.Get(ctx, types.NamespacedName{Name: "test-pdb", Namespace: "default"}, &currentPDB)
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestRecyclePVCsForMissingPods_DeletesPVCForMissingOrdinal(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Spec.NodeMaintenanceWindow = &redisv1.NodeMaintenanceWindow{
		InProgress: true,
		ReusePVC:   boolPtr(false),
	}

	pod0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-0",
				redisv1.LabelRole:     redisv1.LabelRolePrimary,
			},
		},
	}
	pvc0 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
	}
	pvc1 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-1",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
	}

	r, c := newReconciler(cluster, pod0, pvc0, pvc1)
	ctx := context.Background()

	require.NoError(t, r.recyclePVCsForMissingPods(ctx, cluster))

	var currentPVC0 corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "test-data-0", Namespace: "default"}, &currentPVC0))

	var currentPVC1 corev1.PersistentVolumeClaim
	err := c.Get(ctx, types.NamespacedName{Name: "test-data-1", Namespace: "default"}, &currentPVC1)
	require.Error(t, err)
	assert.True(t, apierrors.IsNotFound(err))
}

func TestReconcile_MaintenanceReusePVCFalseReplacesMissingPodAndStorage(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Spec.NodeMaintenanceWindow = &redisv1.NodeMaintenanceWindow{
		InProgress: true,
		ReusePVC:   boolPtr(false),
	}

	pod0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-0",
				redisv1.LabelRole:     redisv1.LabelRolePrimary,
			},
		},
	}
	pvc0 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
	}
	pvc1 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-1",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
			Annotations: map[string]string{
				"test/marker": "old",
			},
		},
	}

	r, c := newReconciler(cluster, pod0, pvc0, pvc1)
	ctx := context.Background()

	result, err := r.reconcile(ctx, cluster)
	require.NoError(t, err)
	assert.Equal(t, requeueInterval, result.RequeueAfter)

	var recreatedPod corev1.Pod
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "test-1", Namespace: "default"}, &recreatedPod))

	var recreatedPVC corev1.PersistentVolumeClaim
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "test-data-1", Namespace: "default"}, &recreatedPVC))
	assert.NotContains(t, recreatedPVC.Annotations, "test/marker", "recreated PVC should not carry old marker")
}
