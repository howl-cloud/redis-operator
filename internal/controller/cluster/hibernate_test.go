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

func TestIsHibernationEnabled(t *testing.T) {
	tests := []struct {
		name        string
		annotations map[string]string
		expected    bool
	}{
		{"no annotations", nil, false},
		{"no hibernation annotation", map[string]string{"other": "val"}, false},
		{"on", map[string]string{redisv1.AnnotationHibernation: "on"}, true},
		{"true", map[string]string{redisv1.AnnotationHibernation: "true"}, true},
		{"ON uppercase", map[string]string{redisv1.AnnotationHibernation: "ON"}, true},
		{"TRUE uppercase", map[string]string{redisv1.AnnotationHibernation: "TRUE"}, true},
		{"off", map[string]string{redisv1.AnnotationHibernation: "off"}, false},
		{"false", map[string]string{redisv1.AnnotationHibernation: "false"}, false},
		{"empty string", map[string]string{redisv1.AnnotationHibernation: ""}, false},
		{"whitespace true", map[string]string{redisv1.AnnotationHibernation: " true "}, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := &redisv1.RedisCluster{
				ObjectMeta: metav1.ObjectMeta{
					Annotations: tt.annotations,
				},
			}
			assert.Equal(t, tt.expected, isHibernationEnabled(cluster))
		})
	}
}

func TestWasHibernated(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Status: redisv1.RedisClusterStatus{
			Phase: redisv1.ClusterPhaseHibernating,
		},
	}
	assert.True(t, wasHibernated(cluster))

	cluster.Status.Phase = redisv1.ClusterPhaseHealthy
	assert.False(t, wasHibernated(cluster))
}

func TestReconcileHibernation_EnableHibernation(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Annotations = map[string]string{
		redisv1.AnnotationHibernation: "on",
	}
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Status.Phase = redisv1.ClusterPhaseHealthy

	// Create pods for the cluster.
	pod0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-1",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-2",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
	}

	r, c := newReconciler(cluster, pod0, pod1, pod2)
	ctx := context.Background()

	hibernating, err := r.reconcileHibernation(ctx, cluster)
	require.NoError(t, err)
	assert.True(t, hibernating, "should return true when hibernating")

	// Verify all pods are deleted.
	var podList corev1.PodList
	err = c.List(ctx, &podList)
	require.NoError(t, err)
	assert.Empty(t, podList.Items, "all pods should be deleted")

	// Verify status is Hibernating.
	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.ClusterPhaseHibernating, updated.Status.Phase)
	assert.Equal(t, int32(0), updated.Status.ReadyInstances)
	assert.Equal(t, int32(0), updated.Status.Instances)
	assert.Nil(t, updated.Status.InstancesStatus)

	// Verify Hibernated condition is set.
	var foundCondition bool
	for _, c := range updated.Status.Conditions {
		if c.Type == redisv1.ConditionHibernated {
			foundCondition = true
			assert.Equal(t, metav1.ConditionTrue, c.Status)
			assert.Equal(t, "HibernationEnabled", c.Reason)
		}
	}
	assert.True(t, foundCondition, "Hibernated condition should be set")
}

func TestReconcileHibernation_PVCsRetained(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Annotations = map[string]string{
		redisv1.AnnotationHibernation: "true",
	}
	cluster.Status.Phase = redisv1.ClusterPhaseHealthy

	// Create pods and PVCs.
	pod0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
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

	hibernating, err := r.reconcileHibernation(ctx, cluster)
	require.NoError(t, err)
	assert.True(t, hibernating)

	// Pods should be deleted.
	var podList corev1.PodList
	err = c.List(ctx, &podList)
	require.NoError(t, err)
	assert.Empty(t, podList.Items)

	// PVCs should still exist.
	var pvcList corev1.PersistentVolumeClaimList
	err = c.List(ctx, &pvcList)
	require.NoError(t, err)
	assert.Len(t, pvcList.Items, 2, "PVCs should be retained during hibernation")
}

func TestReconcileHibernation_ResumeFromHibernation(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	// Annotation removed but phase is still Hibernating.
	cluster.Status.Phase = redisv1.ClusterPhaseHibernating
	cluster.Status.Conditions = []metav1.Condition{
		{
			Type:   redisv1.ConditionHibernated,
			Status: metav1.ConditionTrue,
			Reason: "HibernationEnabled",
		},
	}

	r, c := newReconciler(cluster)
	ctx := context.Background()

	hibernating, err := r.reconcileHibernation(ctx, cluster)
	require.NoError(t, err)
	assert.False(t, hibernating, "should return false when resuming")

	// Verify phase is set to Creating (to trigger normal reconciliation).
	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.ClusterPhaseCreating, updated.Status.Phase)

	// Verify Hibernated condition is now False.
	for _, cond := range updated.Status.Conditions {
		if cond.Type == redisv1.ConditionHibernated {
			assert.Equal(t, metav1.ConditionFalse, cond.Status)
			assert.Equal(t, "HibernationDisabled", cond.Reason)
		}
	}
}

func TestReconcileHibernation_NotHibernating(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	// No hibernation annotation, phase is Healthy.
	cluster.Status.Phase = redisv1.ClusterPhaseHealthy

	r, _ := newReconciler(cluster)
	ctx := context.Background()

	hibernating, err := r.reconcileHibernation(ctx, cluster)
	require.NoError(t, err)
	assert.False(t, hibernating, "should return false when not hibernating")
}

func TestReconcileHibernation_ClearsLeaderServiceSelector(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Annotations = map[string]string{
		redisv1.AnnotationHibernation: "on",
	}
	cluster.Status.Phase = redisv1.ClusterPhaseHealthy
	cluster.Status.CurrentPrimary = "test-0"

	// Create the leader service.
	leaderSvc := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-leader",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
		Spec: corev1.ServiceSpec{
			Selector: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-0",
			},
			Ports: []corev1.ServicePort{
				{Name: "redis", Port: 6379},
			},
		},
	}

	r, c := newReconciler(cluster, leaderSvc)
	ctx := context.Background()

	hibernating, err := r.reconcileHibernation(ctx, cluster)
	require.NoError(t, err)
	assert.True(t, hibernating)

	// Verify leader service selector is cleared.
	var svc corev1.Service
	err = c.Get(ctx, types.NamespacedName{Name: "test-leader", Namespace: "default"}, &svc)
	require.NoError(t, err)
	assert.Equal(t, "none", svc.Spec.Selector[redisv1.LabelInstance],
		"leader service selector should point to 'none' during hibernation")
}

func TestSetCondition_AddNew(t *testing.T) {
	var conditions []metav1.Condition
	now := metav1.Now()
	setCondition(&conditions, metav1.Condition{
		Type:               "TestCondition",
		Status:             metav1.ConditionTrue,
		LastTransitionTime: now,
		Reason:             "TestReason",
	})
	require.Len(t, conditions, 1)
	assert.Equal(t, "TestCondition", conditions[0].Type)
	assert.Equal(t, metav1.ConditionTrue, conditions[0].Status)
}

func TestSetCondition_UpdateExisting(t *testing.T) {
	now := metav1.Now()
	conditions := []metav1.Condition{
		{
			Type:               "TestCondition",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: now,
			Reason:             "OldReason",
		},
	}
	setCondition(&conditions, metav1.Condition{
		Type:               "TestCondition",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: now,
		Reason:             "NewReason",
	})
	require.Len(t, conditions, 1)
	assert.Equal(t, metav1.ConditionFalse, conditions[0].Status)
	assert.Equal(t, "NewReason", conditions[0].Reason)
}
