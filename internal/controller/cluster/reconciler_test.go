package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(redisv1.AddToScheme(s))
	return s
}

func boolPtr(b bool) *bool {
	return &b
}

func newTestCluster(name, namespace string, instances int32) *redisv1.RedisCluster {
	return &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: redisv1.RedisClusterSpec{
			Instances:                 instances,
			Mode:                      redisv1.ClusterModeStandalone,
			ImageName:                 "redis:7.2",
			EnablePodDisruptionBudget: boolPtr(true),
			Storage: redisv1.StorageSpec{
				Size: resource.MustParse("1Gi"),
			},
		},
	}
}

func newReconciler(objs ...client.Object) (*ClusterReconciler, client.Client) {
	scheme := testScheme()
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisCluster{})
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	c := builder.Build()
	recorder := record.NewFakeRecorder(100)
	return NewClusterReconciler(c, scheme, recorder, 0), c
}

type immutableRoleRefClient struct {
	client.Client
}

func (c *immutableRoleRefClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	binding, ok := obj.(*rbacv1.RoleBinding)
	if !ok {
		return c.Client.Update(ctx, obj, opts...)
	}

	var existing rbacv1.RoleBinding
	err := c.Get(ctx, types.NamespacedName{Name: binding.Name, Namespace: binding.Namespace}, &existing)
	if err == nil && existing.RoleRef != binding.RoleRef {
		return apierrors.NewInvalid(
			schema.GroupKind{Group: rbacv1.GroupName, Kind: "RoleBinding"},
			binding.Name,
			field.ErrorList{
				field.Invalid(field.NewPath("roleRef"), binding.RoleRef, "field is immutable"),
			},
		)
	}

	return c.Client.Update(ctx, obj, opts...)
}

func TestNewClusterReconciler_UsesOperatorImageFromEnv(t *testing.T) {
	t.Setenv("OPERATOR_IMAGE_NAME", "example/redis-operator:test")

	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(1)

	r := NewClusterReconciler(c, scheme, recorder, 0)
	assert.Equal(t, "example/redis-operator:test", r.OperatorImage)
	assert.Equal(t, defaultMaxConcurrentReconciles, r.MaxConcurrentReconciles)
}

func TestNewClusterReconciler_DefaultOperatorImage(t *testing.T) {
	t.Setenv("OPERATOR_IMAGE_NAME", "")

	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(1)

	r := NewClusterReconciler(c, scheme, recorder, 0)
	assert.Equal(t, defaultOperatorImage, r.OperatorImage)
	assert.Equal(t, defaultMaxConcurrentReconciles, r.MaxConcurrentReconciles)
}

func TestNewClusterReconciler_CustomMaxConcurrentReconciles(t *testing.T) {
	scheme := testScheme()
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	recorder := record.NewFakeRecorder(1)

	r := NewClusterReconciler(c, scheme, recorder, 9)
	assert.Equal(t, 9, r.MaxConcurrentReconciles)
}

func TestReconcilePDB_Creates(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.reconcilePDB(ctx, cluster)
	require.NoError(t, err)

	// Verify PDB was created.
	var pdb policyv1.PodDisruptionBudget
	err = c.Get(ctx, types.NamespacedName{Name: "test-pdb", Namespace: "default"}, &pdb)
	require.NoError(t, err)
	assert.Equal(t, "test", pdb.Labels[redisv1.LabelCluster])
	require.NotNil(t, pdb.Spec.MinAvailable)
	assert.Equal(t, 2, pdb.Spec.MinAvailable.IntValue()) // max(1, 3-1) = 2
}

func TestReconcilePDB_Disabled(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Spec.EnablePodDisruptionBudget = boolPtr(false)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.reconcilePDB(ctx, cluster)
	require.NoError(t, err)

	// Verify no PDB exists.
	var pdb policyv1.PodDisruptionBudget
	err = c.Get(ctx, types.NamespacedName{Name: "test-pdb", Namespace: "default"}, &pdb)
	assert.True(t, err != nil, "PDB should not exist")
}

func TestReconcilePDB_DeletesWhenDisabled(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)

	// First create a PDB.
	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.reconcilePDB(ctx, cluster)
	require.NoError(t, err)

	// Now disable PDB.
	cluster.Spec.EnablePodDisruptionBudget = boolPtr(false)
	err = r.reconcilePDB(ctx, cluster)
	require.NoError(t, err)

	// PDB should be gone.
	var pdb policyv1.PodDisruptionBudget
	err = c.Get(ctx, types.NamespacedName{Name: "test-pdb", Namespace: "default"}, &pdb)
	assert.True(t, err != nil, "PDB should be deleted")
}

func TestReconcileServices_CreatesThreeServices(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.reconcileServices(ctx, cluster)
	require.NoError(t, err)

	// Verify leader service.
	var leaderSvc corev1.Service
	err = c.Get(ctx, types.NamespacedName{Name: "test-leader", Namespace: "default"}, &leaderSvc)
	require.NoError(t, err)
	assert.Equal(t, "test", leaderSvc.Labels[redisv1.LabelCluster])

	// Verify replica service.
	var replicaSvc corev1.Service
	err = c.Get(ctx, types.NamespacedName{Name: "test-replica", Namespace: "default"}, &replicaSvc)
	require.NoError(t, err)

	// Verify any service.
	var anySvc corev1.Service
	err = c.Get(ctx, types.NamespacedName{Name: "test-any", Namespace: "default"}, &anySvc)
	require.NoError(t, err)
}

func TestReconcileServices_LeaderSelector(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-0"
	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.reconcileServices(ctx, cluster)
	require.NoError(t, err)

	// Verify leader service points to primary.
	var leaderSvc corev1.Service
	err = c.Get(ctx, types.NamespacedName{Name: "test-leader", Namespace: "default"}, &leaderSvc)
	require.NoError(t, err)
	assert.Equal(t, "test-0", leaderSvc.Spec.Selector[redisv1.LabelInstance])
}

func TestReconcileSecrets_AutoGeneratesAuth(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.reconcileSecrets(ctx, cluster)
	require.NoError(t, err)

	// Verify auth secret was created.
	var secret corev1.Secret
	err = c.Get(ctx, types.NamespacedName{Name: "test-auth", Namespace: "default"}, &secret)
	require.NoError(t, err)
	assert.NotEmpty(t, secret.Data["password"])
	assert.Equal(t, "test", secret.Labels[redisv1.LabelCluster])
}

func TestReconcileSecrets_ExistingAuthNotOverwritten(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "my-auth"}

	existingSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:            "my-auth",
			Namespace:       "default",
			ResourceVersion: "100",
		},
		Data: map[string][]byte{
			"password": []byte("my-password"),
		},
	}

	r, _ := newReconciler(cluster, existingSecret)
	ctx := context.Background()

	err := r.reconcileSecrets(ctx, cluster)
	require.NoError(t, err)
}

func TestReconcilePVCs_CreatesRequired(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	_, err := r.reconcilePVCs(ctx, cluster)
	require.NoError(t, err)

	// Verify PVCs were created.
	var pvc0 corev1.PersistentVolumeClaim
	err = c.Get(ctx, types.NamespacedName{Name: "test-data-0", Namespace: "default"}, &pvc0)
	require.NoError(t, err)
	assert.Equal(t, "test", pvc0.Labels[redisv1.LabelCluster])

	var pvc1 corev1.PersistentVolumeClaim
	err = c.Get(ctx, types.NamespacedName{Name: "test-data-1", Namespace: "default"}, &pvc1)
	require.NoError(t, err)
}

func TestReconcilePVCs_ResizesExistingClaims(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Spec.Storage.Size = resource.MustParse("2Gi")
	storageClassName := "expandable"
	cluster.Spec.Storage.StorageClassName = &storageClassName

	allowExpansion := true
	storageClass := &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: storageClassName},
		AllowVolumeExpansion: &allowExpansion,
	}

	pvc0 := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}
	pvc1 := pvc0.DeepCopy()
	pvc1.Name = "test-data-1"

	r, c := newReconciler(cluster, storageClass, pvc0, pvc1)
	ctx := context.Background()

	pendingResizePVCs, err := r.reconcilePVCs(ctx, cluster)
	require.NoError(t, err)
	assert.Empty(t, pendingResizePVCs)

	var updatedPVC corev1.PersistentVolumeClaim
	err = c.Get(ctx, types.NamespacedName{Name: "test-data-0", Namespace: "default"}, &updatedPVC)
	require.NoError(t, err)
	updatedStorage := updatedPVC.Spec.Resources.Requests[corev1.ResourceStorage]
	assert.Equal(t, 0, updatedStorage.Cmp(resource.MustParse("2Gi")))

	var updatedCluster redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updatedCluster)
	require.NoError(t, err)
	foundResizeCondition := false
	for i := range updatedCluster.Status.Conditions {
		condition := updatedCluster.Status.Conditions[i]
		if condition.Type == redisv1.ConditionPVCResizeInProgress && condition.Status == metav1.ConditionTrue {
			foundResizeCondition = true
			break
		}
	}
	assert.True(t, foundResizeCondition, "expected PVCResizeInProgress=true condition")
}

func TestReconcilePVCs_ResizeBlockedWhenStorageClassDisallowsExpansion(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Storage.Size = resource.MustParse("2Gi")
	storageClassName := "no-expand"
	cluster.Spec.Storage.StorageClassName = &storageClassName

	allowExpansion := false
	storageClass := &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: storageClassName},
		AllowVolumeExpansion: &allowExpansion,
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			StorageClassName: &storageClassName,
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}

	r, c, recorder := newReconcilerWithRecorder(cluster, storageClass, pvc)
	ctx := context.Background()

	pendingResizePVCs, err := r.reconcilePVCs(ctx, cluster)
	require.NoError(t, err)
	assert.Empty(t, pendingResizePVCs)

	var updatedPVC corev1.PersistentVolumeClaim
	err = c.Get(ctx, types.NamespacedName{Name: "test-data-0", Namespace: "default"}, &updatedPVC)
	require.NoError(t, err)
	updatedStorage := updatedPVC.Spec.Resources.Requests[corev1.ResourceStorage]
	assert.Equal(t, 0, updatedStorage.Cmp(resource.MustParse("1Gi")))

	events := drainEvents(recorder)
	assertContainsEvent(t, events, "Warning", "PVCResizeFailed", "does not allow volume expansion")
}

func TestReconcilePVCs_ReturnsPendingFilesystemResizePVCs(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
		Status: corev1.PersistentVolumeClaimStatus{
			Conditions: []corev1.PersistentVolumeClaimCondition{
				{
					Type:   corev1.PersistentVolumeClaimFileSystemResizePending,
					Status: corev1.ConditionTrue,
				},
			},
		},
	}

	r, c := newReconciler(cluster, pvc)
	ctx := context.Background()

	pendingResizePVCs, err := r.reconcilePVCs(ctx, cluster)
	require.NoError(t, err)
	_, found := pendingResizePVCs["test-data-0"]
	assert.True(t, found, "expected test-data-0 to require restart")

	var updatedCluster redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updatedCluster)
	require.NoError(t, err)
	foundResizeCondition := false
	for i := range updatedCluster.Status.Conditions {
		condition := updatedCluster.Status.Conditions[i]
		if condition.Type == redisv1.ConditionPVCResizeInProgress && condition.Status == metav1.ConditionTrue {
			foundResizeCondition = true
			break
		}
	}
	assert.True(t, foundResizeCondition, "expected PVCResizeInProgress=true condition")
}

func TestRestartPodsForPendingResize_DeletesHighestOrdinalReplica(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-0"

	pod0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "test-data-0",
						},
					},
				},
			},
			Containers: []corev1.Container{{Name: "redis", Image: "redis:7.2"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-1",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "test-data-1",
						},
					},
				},
			},
			Containers: []corev1.Container{{Name: "redis", Image: "redis:7.2"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-2",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "test-data-2",
						},
					},
				},
			},
			Containers: []corev1.Container{{Name: "redis", Image: "redis:7.2"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}

	r, c := newReconciler(cluster, pod0, pod1, pod2)
	ctx := context.Background()

	stop, err := r.restartPodsForPendingResize(ctx, cluster, map[string]struct{}{
		"test-data-1": {},
		"test-data-2": {},
	})
	require.NoError(t, err)
	assert.True(t, stop, "expected one restart action")

	var deleted corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-2", Namespace: "default"}, &deleted)
	assert.True(t, apierrors.IsNotFound(err), "expected highest ordinal pending replica to be deleted first")

	var remaining corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-1", Namespace: "default"}, &remaining)
	require.NoError(t, err)
}

func TestRestartPodsForPendingResize_WaitsWhenReplicaNotReady(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"

	primary := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "test-data-0",
						},
					},
				},
			},
			Containers: []corev1.Container{{Name: "redis", Image: "redis:7.2"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			Conditions: []corev1.PodCondition{
				{Type: corev1.PodReady, Status: corev1.ConditionTrue},
			},
		},
	}
	replicaNotReady := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-1",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "test-data-1",
						},
					},
				},
			},
			Containers: []corev1.Container{{Name: "redis", Image: "redis:7.2"}},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodPending,
		},
	}

	r, c := newReconciler(cluster, primary, replicaNotReady)
	ctx := context.Background()

	stop, err := r.restartPodsForPendingResize(ctx, cluster, map[string]struct{}{
		"test-data-1": {},
	})
	require.NoError(t, err)
	assert.True(t, stop, "expected reconcile to pause while waiting for readiness")

	var stillThere corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-1", Namespace: "default"}, &stillThere)
	require.NoError(t, err)
}

func TestReconcilePods_ScaleUp(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	// Pre-create PVCs so pods can reference them.
	for i := 0; i < 2; i++ {
		pvc := &corev1.PersistentVolumeClaim{
			ObjectMeta: metav1.ObjectMeta{
				Name:      pvcNameForIndex("test", i),
				Namespace: "default",
				Labels:    map[string]string{redisv1.LabelCluster: "test"},
			},
		}
		require.NoError(t, c.Create(ctx, pvc))
	}

	err := r.reconcilePods(ctx, cluster)
	require.NoError(t, err)

	// Verify pods were created.
	var pod0 corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-0", Namespace: "default"}, &pod0)
	require.NoError(t, err)
	assert.Equal(t, "test", pod0.Labels[redisv1.LabelCluster])
	assert.Equal(t, "test-0", pod0.Labels[redisv1.LabelInstance])
	// First pod should be primary since no CurrentPrimary is set.
	assert.Equal(t, redisv1.LabelRolePrimary, pod0.Labels[redisv1.LabelRole])

	var pod1 corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-1", Namespace: "default"}, &pod1)
	require.NoError(t, err)
	assert.Equal(t, redisv1.LabelRoleReplica, pod1.Labels[redisv1.LabelRole])
}

func TestReconcilePods_SetInitialPrimary(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	// Pre-create PVC.
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
	}
	require.NoError(t, c.Create(ctx, pvc))

	err := r.reconcilePods(ctx, cluster)
	require.NoError(t, err)

	// Re-fetch the cluster to see the status update.
	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, "test-0", updated.Status.CurrentPrimary)
}

func TestReconcilePods_ScaleDown(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Status.CurrentPrimary = "test-0"

	// Create 2 existing pods.
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
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-1",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-1",
				redisv1.LabelRole:     redisv1.LabelRoleReplica,
			},
		},
	}

	r, c := newReconciler(cluster, pod0, pod1)
	ctx := context.Background()

	err := r.reconcilePods(ctx, cluster)
	require.NoError(t, err)

	// Verify pod-1 was deleted (scale down from 2 to 1).
	var deletedPod corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-1", Namespace: "default"}, &deletedPod)
	assert.True(t, err != nil, "pod test-1 should be deleted")

	// Primary pod should remain.
	var primaryPod corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-0", Namespace: "default"}, &primaryPod)
	assert.NoError(t, err)
}

func TestReconcilePods_RecreatesMissingOrdinal(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-0"

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
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-2",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-2",
				redisv1.LabelRole:     redisv1.LabelRoleReplica,
			},
		},
	}

	r, c := newReconciler(cluster, pod0, pod2)
	ctx := context.Background()

	err := r.reconcilePods(ctx, cluster)
	require.NoError(t, err)

	var recreated corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-1", Namespace: "default"}, &recreated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.LabelRoleReplica, recreated.Labels[redisv1.LabelRole])
}

func TestReconcilePods_RecreatesMissingCurrentPrimaryAsPrimary(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-1"

	pod0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-0",
				redisv1.LabelRole:     redisv1.LabelRoleReplica,
			},
		},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-2",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster:  "test",
				redisv1.LabelInstance: "test-2",
				redisv1.LabelRole:     redisv1.LabelRoleReplica,
			},
		},
	}

	r, c := newReconciler(cluster, pod0, pod2)
	ctx := context.Background()

	err := r.reconcilePods(ctx, cluster)
	require.NoError(t, err)

	var recreatedPrimary corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-1", Namespace: "default"}, &recreatedPrimary)
	require.NoError(t, err)
	assert.Equal(t, redisv1.LabelRolePrimary, recreatedPrimary.Labels[redisv1.LabelRole])
}

func TestReconcileServiceAccount_Creates(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.reconcileServiceAccount(ctx, cluster)
	require.NoError(t, err)

	var sa corev1.ServiceAccount
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &sa)
	require.NoError(t, err)
	assert.Equal(t, "test", sa.Labels[redisv1.LabelCluster])
	assert.True(t, metav1.IsControlledBy(&sa, cluster))
}

func TestReconcileServiceAccount_Idempotent(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	r, _ := newReconciler(cluster)
	ctx := context.Background()

	// Call twice.
	require.NoError(t, r.reconcileServiceAccount(ctx, cluster))
	require.NoError(t, r.reconcileServiceAccount(ctx, cluster))
}

func TestReconcileRBAC_CreatesRoleAndRoleBinding(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.reconcileRBAC(ctx, cluster)
	require.NoError(t, err)

	var role rbacv1.Role
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &role)
	require.NoError(t, err)
	assert.Equal(t, "test", role.Labels[redisv1.LabelCluster])
	assert.Equal(t, instanceManagerRoleRules(), role.Rules)
	assert.True(t, metav1.IsControlledBy(&role, cluster))

	var binding rbacv1.RoleBinding
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &binding)
	require.NoError(t, err)
	assert.Equal(t, "test", binding.Labels[redisv1.LabelCluster])
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, "test", binding.Subjects[0].Name)
	assert.Equal(t, "default", binding.Subjects[0].Namespace)
	assert.Equal(t, "test", binding.RoleRef.Name)
	assert.True(t, metav1.IsControlledBy(&binding, cluster))
}

func TestReconcileRBAC_UpdatesDriftedRoleAndRoleBinding(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	staleRole := &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "other",
			},
		},
		Rules: []rbacv1.PolicyRule{
			{
				APIGroups: []string{"redis.io"},
				Resources: []string{"redisclusters"},
				Verbs:     []string{"get"},
			},
		},
	}
	staleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "other",
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      "wrong-name",
				Namespace: "default",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     "test",
		},
	}

	r, c := newReconciler(cluster, staleRole, staleBinding)
	ctx := context.Background()

	err := r.reconcileRBAC(ctx, cluster)
	require.NoError(t, err)

	var role rbacv1.Role
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &role)
	require.NoError(t, err)
	assert.Equal(t, "test", role.Labels[redisv1.LabelCluster])
	assert.Equal(t, instanceManagerRoleRules(), role.Rules)
	assert.True(t, metav1.IsControlledBy(&role, cluster))

	var binding rbacv1.RoleBinding
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &binding)
	require.NoError(t, err)
	assert.Equal(t, "test", binding.Labels[redisv1.LabelCluster])
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, "test", binding.Subjects[0].Name)
	assert.Equal(t, "default", binding.Subjects[0].Namespace)
	assert.Equal(t, "test", binding.RoleRef.Name)
	assert.True(t, metav1.IsControlledBy(&binding, cluster))
}

func TestReconcileRBAC_RecreatesRoleBindingWhenRoleRefDrifts(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	staleBinding := &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "other",
			},
		},
		Subjects: []rbacv1.Subject{
			{
				Kind:      rbacv1.ServiceAccountKind,
				Name:      "wrong-name",
				Namespace: "default",
			},
		},
		RoleRef: rbacv1.RoleRef{
			APIGroup: rbacv1.GroupName,
			Kind:     "Role",
			Name:     "wrong-role",
		},
	}

	baseReconciler, fakeClient := newReconciler(cluster, staleBinding)
	r := &ClusterReconciler{
		Client:        &immutableRoleRefClient{Client: fakeClient},
		Scheme:        baseReconciler.Scheme,
		Recorder:      baseReconciler.Recorder,
		OperatorImage: baseReconciler.OperatorImage,
	}
	ctx := context.Background()

	err := r.reconcileRBAC(ctx, cluster)
	require.NoError(t, err)

	var binding rbacv1.RoleBinding
	err = fakeClient.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &binding)
	require.NoError(t, err)
	assert.Equal(t, "test", binding.Labels[redisv1.LabelCluster])
	assert.Equal(t, "test", binding.RoleRef.Name)
	require.Len(t, binding.Subjects, 1)
	assert.Equal(t, "test", binding.Subjects[0].Name)
	assert.Equal(t, "default", binding.Subjects[0].Namespace)
	assert.True(t, metav1.IsControlledBy(&binding, cluster))
}

func TestReconcileConfigMap_Creates(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Redis = map[string]string{
		"maxmemory":        "256mb",
		"maxmemory-policy": "allkeys-lru",
	}
	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.reconcileConfigMap(ctx, cluster)
	require.NoError(t, err)

	var cm corev1.ConfigMap
	err = c.Get(ctx, types.NamespacedName{Name: "test-config", Namespace: "default"}, &cm)
	require.NoError(t, err)
	assert.Contains(t, cm.Data["redis.conf"], "port 6379")
	assert.Contains(t, cm.Data["redis.conf"], "appendonly yes")
	assert.Contains(t, cm.Data["redis.conf"], "aof-use-rdb-preamble yes")
	assert.Contains(t, cm.Data["redis.conf"], "appenddirname appendonlydir")
	// User-specified params should be present (order may vary).
	assert.Contains(t, cm.Data["redis.conf"], "maxmemory 256mb")
	assert.Contains(t, cm.Data["redis.conf"], "maxmemory-policy allkeys-lru")
}

func TestReconcilePVCs_DetectsDangling(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Status.CurrentPrimary = "test-0"

	// Create a pod that references PVC test-data-0.
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
		Spec: corev1.PodSpec{
			Volumes: []corev1.Volume{
				{
					Name: "data",
					VolumeSource: corev1.VolumeSource{
						PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
							ClaimName: "test-data-0",
						},
					},
				},
			},
			Containers: []corev1.Container{{Name: "redis", Image: "redis:7.2"}},
		},
	}

	// Create a dangling PVC (test-data-5 with no matching pod).
	danglingPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-5",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
	}

	r, c := newReconciler(cluster, pod, danglingPVC)
	ctx := context.Background()

	_, err := r.reconcilePVCs(ctx, cluster)
	require.NoError(t, err)

	// Re-fetch cluster status.
	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)

	assert.Contains(t, updated.Status.DanglingPVC, "test-data-5")
}

func TestUsesSecret(t *testing.T) {
	r := &ClusterReconciler{}
	cluster := &redisv1.RedisCluster{
		Status: redisv1.RedisClusterStatus{
			SecretsResourceVersion: map[string]string{
				"my-auth": "100",
				"my-tls":  "200",
			},
		},
	}

	assert.True(t, r.UsesSecret(cluster, "my-auth"))
	assert.True(t, r.UsesSecret(cluster, "my-tls"))
	assert.False(t, r.UsesSecret(cluster, "unknown-secret"))
}

func TestCreatePod_WithSecrets(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "auth-secret"}
	cluster.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls-secret"}
	cluster.Spec.CASecret = &redisv1.LocalObjectReference{Name: "ca-secret"}

	// Pre-create PVC.
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
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

	// Verify projected volume exists.
	foundProjected := false
	foundTLSVolume := false
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == projectedVolumeName {
			foundProjected = true
			require.NotNil(t, vol.Projected)
			assert.Len(t, vol.Projected.Sources, 1) // auth only
			require.NotNil(t, vol.Projected.Sources[0].Secret)
			assert.Equal(t, "auth-secret", vol.Projected.Sources[0].Secret.Name)
			assert.Equal(t, []corev1.KeyToPath{
				{Key: "password", Path: "auth-secret/password"},
			}, vol.Projected.Sources[0].Secret.Items)
		}
		if vol.Name == tlsVolumeName {
			foundTLSVolume = true
			require.NotNil(t, vol.Projected)
			require.Len(t, vol.Projected.Sources, 2)

			require.NotNil(t, vol.Projected.Sources[0].Secret)
			assert.Equal(t, "tls-secret", vol.Projected.Sources[0].Secret.Name)
			assert.Equal(t, []corev1.KeyToPath{
				{Key: "tls.crt", Path: "tls.crt"},
				{Key: "tls.key", Path: "tls.key"},
			}, vol.Projected.Sources[0].Secret.Items)

			require.NotNil(t, vol.Projected.Sources[1].Secret)
			assert.Equal(t, "ca-secret", vol.Projected.Sources[1].Secret.Name)
			assert.Equal(t, []corev1.KeyToPath{
				{Key: "ca.crt", Path: "ca.crt"},
			}, vol.Projected.Sources[1].Secret.Items)
		}
	}
	assert.True(t, foundProjected, "projected volume should be present")
	assert.True(t, foundTLSVolume, "tls volume should be present")

	// Verify mount.
	foundMount := false
	foundTLSMount := false
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.Name == projectedVolumeName {
			foundMount = true
			assert.Equal(t, projectedMountPath, mount.MountPath)
		}
		if mount.Name == tlsVolumeName {
			foundTLSMount = true
			assert.Equal(t, tlsMountPath, mount.MountPath)
			assert.True(t, mount.ReadOnly)
		}
	}
	assert.True(t, foundMount, "projected volume mount should be present")
	assert.True(t, foundTLSMount, "tls volume mount should be present")
}

func TestCreatePod_WithoutSecrets(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	// No secrets configured.

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
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

	// No projected volume should exist.
	for _, vol := range pod.Spec.Volumes {
		assert.NotEqual(t, projectedVolumeName, vol.Name)
	}
}

func TestCreateSentinelPod_WithAuthSecretProjectsPassword(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "auth-secret"}

	r, c := newReconciler(cluster)
	ctx := context.Background()

	err := r.createSentinelPod(ctx, cluster, "test-sentinel-0")
	require.NoError(t, err)

	var pod corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-sentinel-0", Namespace: "default"}, &pod)
	require.NoError(t, err)

	foundProjected := false
	for _, vol := range pod.Spec.Volumes {
		if vol.Name != projectedVolumeName {
			continue
		}
		foundProjected = true
		require.NotNil(t, vol.Projected)
		require.Len(t, vol.Projected.Sources, 1)
		require.NotNil(t, vol.Projected.Sources[0].Secret)
		assert.Equal(t, "auth-secret", vol.Projected.Sources[0].Secret.Name)
		assert.Equal(t, []corev1.KeyToPath{
			{Key: "password", Path: "auth-secret/password"},
		}, vol.Projected.Sources[0].Secret.Items)
	}
	assert.True(t, foundProjected, "projected volume should be present on sentinel pod")

	foundMount := false
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.Name == projectedVolumeName && mount.MountPath == projectedMountPath {
			foundMount = true
			break
		}
	}
	assert.True(t, foundMount, "projected volume mount should be present on sentinel pod")
	require.NotNil(t, pod.Spec.SecurityContext)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot)
	assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsUser)
	assert.Equal(t, int64(999), *pod.Spec.SecurityContext.RunAsUser)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsGroup)
	assert.Equal(t, int64(999), *pod.Spec.SecurityContext.RunAsGroup)
	require.NotNil(t, pod.Spec.SecurityContext.FSGroup)
	assert.Equal(t, int64(999), *pod.Spec.SecurityContext.FSGroup)
	require.NotNil(t, pod.Spec.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.Spec.SecurityContext.SeccompProfile.Type)

	require.Len(t, pod.Spec.Containers, 1)
	require.NotNil(t, pod.Spec.Containers[0].SecurityContext)
	require.NotNil(t, pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *pod.Spec.Containers[0].SecurityContext.AllowPrivilegeEscalation)
	require.NotNil(t, pod.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem)
	assert.False(t, *pod.Spec.Containers[0].SecurityContext.ReadOnlyRootFilesystem)
}

func TestCreatePod_ContainerSpec(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
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

	// Check container configuration.
	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assert.Equal(t, "redis", container.Name)
	assert.Equal(t, "redis:7.2", container.Image)
	assert.Equal(t, []string{instanceManagerBinaryPath, "instance"}, container.Command)

	// Verify ports.
	require.Len(t, container.Ports, 2)
	portNames := make(map[string]int32)
	for _, p := range container.Ports {
		portNames[p.Name] = p.ContainerPort
	}
	assert.Equal(t, int32(6379), portNames["redis"])
	assert.Equal(t, int32(8080), portNames["http"])

	// Verify probes exist.
	require.NotNil(t, container.LivenessProbe)
	require.NotNil(t, container.ReadinessProbe)
	assert.Equal(t, "/healthz", container.LivenessProbe.HTTPGet.Path)
	assert.Equal(t, int32(12), container.LivenessProbe.TimeoutSeconds)
	assert.Equal(t, "/readyz", container.ReadinessProbe.HTTPGet.Path)

	// Verify env vars.
	envMap := make(map[string]string)
	for _, e := range container.Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, "test-0", envMap["POD_NAME"])
	assert.Equal(t, "test", envMap["CLUSTER_NAME"])
	assert.Equal(t, "default", envMap["POD_NAMESPACE"])

	require.NotNil(t, pod.Spec.SecurityContext)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot)
	assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsUser)
	assert.Equal(t, int64(999), *pod.Spec.SecurityContext.RunAsUser)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsGroup)
	assert.Equal(t, int64(999), *pod.Spec.SecurityContext.RunAsGroup)
	require.NotNil(t, pod.Spec.SecurityContext.FSGroup)
	assert.Equal(t, int64(999), *pod.Spec.SecurityContext.FSGroup)
	require.NotNil(t, pod.Spec.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.Spec.SecurityContext.SeccompProfile.Type)

	require.NotNil(t, container.SecurityContext)
	require.NotNil(t, container.SecurityContext.RunAsNonRoot)
	assert.True(t, *container.SecurityContext.RunAsNonRoot)
	require.NotNil(t, container.SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
	require.NotNil(t, container.SecurityContext.ReadOnlyRootFilesystem)
	assert.False(t, *container.SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, container.SecurityContext.Capabilities)
	assert.Contains(t, container.SecurityContext.Capabilities.Drop, corev1.Capability("ALL"))

	// Verify init container.
	require.Len(t, pod.Spec.InitContainers, 1)
	assert.Equal(t, "copy-manager", pod.Spec.InitContainers[0].Name)
	assert.Equal(t, r.OperatorImage, pod.Spec.InitContainers[0].Image)
	require.NotNil(t, pod.Spec.InitContainers[0].SecurityContext)
	require.NotNil(t, pod.Spec.InitContainers[0].SecurityContext.RunAsNonRoot)
	assert.True(t, *pod.Spec.InitContainers[0].SecurityContext.RunAsNonRoot)
	require.NotNil(t, pod.Spec.InitContainers[0].SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *pod.Spec.InitContainers[0].SecurityContext.AllowPrivilegeEscalation)
	require.NotNil(t, pod.Spec.InitContainers[0].SecurityContext.ReadOnlyRootFilesystem)
	assert.False(t, *pod.Spec.InitContainers[0].SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, pod.Spec.InitContainers[0].SecurityContext.Capabilities)
	assert.Contains(t, pod.Spec.InitContainers[0].SecurityContext.Capabilities.Drop, corev1.Capability("ALL"))
}

func TestCreatePod_LivenessTimeoutFollowsIsolationConfig(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	apiTimeout := metav1.Duration{Duration: 7 * time.Second}
	peerTimeout := metav1.Duration{Duration: 11 * time.Second}
	cluster.Spec.PrimaryIsolation = &redisv1.PrimaryIsolationSpec{
		APIServerTimeout: &apiTimeout,
		PeerTimeout:      &peerTimeout,
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
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
	require.Len(t, pod.Spec.Containers, 1)
	require.NotNil(t, pod.Spec.Containers[0].LivenessProbe)
	assert.Equal(t, int32(20), pod.Spec.Containers[0].LivenessProbe.TimeoutSeconds)
}

func TestShouldRestoreFromBackup_OnlyFirstBootstrapPrimary(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: "seed-backup"}

	tests := []struct {
		name               string
		podName            string
		index              int
		currentPrimaryName string
		want               bool
	}{
		{
			name:    "initial ordinal-0 pod restores",
			podName: "test-0",
			index:   0,
			want:    true,
		},
		{
			name:    "replica does not restore on bootstrap",
			podName: "test-1",
			index:   1,
			want:    false,
		},
		{
			name:               "primary recreation does not restore once primary is set",
			podName:            "test-0",
			index:              0,
			currentPrimaryName: "test-0",
			want:               false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster.Status.CurrentPrimary = tt.currentPrimaryName
			got := shouldRestoreFromBackup(cluster, tt.podName, tt.index)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestShouldRestoreFromBackup_ClusterModeOnlyBeforeBootstrapCompletion(t *testing.T) {
	cluster := newTestCluster("test", "default", 0)
	cluster.Spec.Mode = redisv1.ClusterModeCluster
	cluster.Spec.Shards = 3
	cluster.Spec.ReplicasPerShard = 1
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: "seed-backup"}

	tests := []struct {
		name               string
		index              int
		bootstrapCompleted bool
		want               bool
	}{
		{
			name:               "shard primary restores during bootstrap",
			index:              0,
			bootstrapCompleted: false,
			want:               true,
		},
		{
			name:               "shard replica does not restore",
			index:              1,
			bootstrapCompleted: false,
			want:               false,
		},
		{
			name:               "primary recreation does not restore once bootstrap is complete",
			index:              2,
			bootstrapCompleted: true,
			want:               false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster.Status.BootstrapCompleted = tt.bootstrapCompleted
			podName := podNameForIndex(cluster.Name, tt.index)
			got := shouldRestoreFromBackup(cluster, podName, tt.index)
			assert.Equal(t, tt.want, got)
		})
	}
}

func TestCreatePod_DoesNotInjectRestoreAfterBootstrap(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: "seed-backup"}
	cluster.Status.CurrentPrimary = "test-0"

	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
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
	require.Len(t, pod.Spec.InitContainers, 1)
	assert.Equal(t, "copy-manager", pod.Spec.InitContainers[0].Name)
}

func TestCreatePod_InjectsRestoreForAOFBackup(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: "seed-backup"}
	cluster.Spec.BackupCredentialsSecret = &redisv1.LocalObjectReference{Name: "backup-creds"}

	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "seed-backup",
			Namespace: "default",
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "source-cluster",
			Method:      redisv1.BackupMethodAOF,
			Destination: &redisv1.BackupDestination{
				S3: &redisv1.S3Destination{
					Bucket: "test-bucket",
					Path:   "backups",
				},
			},
		},
		Status: redisv1.RedisBackupStatus{
			Phase:        redisv1.BackupPhaseCompleted,
			BackupPath:   "s3://test-bucket/backups/seed-backup.aof.tar.gz",
			ArtifactType: redisv1.BackupArtifactTypeAOFArchive,
		},
	}
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
			Namespace: "default",
		},
	}

	r, c := newReconciler(cluster, backup, pvc)
	ctx := context.Background()

	err := r.createPod(ctx, cluster, "test-0", 0, redisv1.LabelRolePrimary)
	require.NoError(t, err)

	var pod corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-0", Namespace: "default"}, &pod)
	require.NoError(t, err)
	require.Len(t, pod.Spec.InitContainers, 2)

	var restoreInit *corev1.Container
	for i := range pod.Spec.InitContainers {
		if pod.Spec.InitContainers[i].Name == restoreDataInitName {
			restoreInit = &pod.Spec.InitContainers[i]
			break
		}
	}
	require.NotNil(t, restoreInit)
	assert.Equal(t, []string{"/manager", "restore"}, restoreInit.Command)
	assert.Contains(t, restoreInit.Args, "--backup-name=seed-backup")
	assert.Contains(t, restoreInit.Args, "--cluster-name=test")
	assert.Contains(t, restoreInit.Args, "--backup-namespace=default")
	assert.Contains(t, restoreInit.Args, "--data-dir=/data")
	assert.Contains(t, restoreInit.VolumeMounts, corev1.VolumeMount{
		Name:      backupCredsVolumeName,
		MountPath: backupCredsMountPath,
		ReadOnly:  true,
	})

	require.NotEmpty(t, pod.Spec.Containers)
	assert.Contains(t, pod.Spec.Containers[0].VolumeMounts, corev1.VolumeMount{
		Name:      backupCredsVolumeName,
		MountPath: backupCredsMountPath,
		ReadOnly:  true,
	})
}

func TestInstanceManagerRoleRules_IncludeRestorePermissions(t *testing.T) {
	rules := instanceManagerRoleRules()

	hasPermission := func(apiGroup, resource, verb string) bool {
		for _, rule := range rules {
			groupMatch := false
			for _, g := range rule.APIGroups {
				if g == apiGroup {
					groupMatch = true
					break
				}
			}
			if !groupMatch {
				continue
			}

			resourceMatch := false
			for _, r := range rule.Resources {
				if r == resource {
					resourceMatch = true
					break
				}
			}
			if !resourceMatch {
				continue
			}

			for _, v := range rule.Verbs {
				if v == verb {
					return true
				}
			}
		}
		return false
	}

	assert.True(t, hasPermission("redis.io", "redisbackups", "get"))
	assert.True(t, hasPermission("", "pods", "get"))
	assert.True(t, hasPermission("", "pods", "list"))
	assert.True(t, hasPermission("", "secrets", "get"))
}

func TestReconcilePDB_UpdatesMinAvailable(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	// Create PDB.
	require.NoError(t, r.reconcilePDB(ctx, cluster))

	// Scale to 5.
	cluster.Spec.Instances = 5
	require.NoError(t, r.reconcilePDB(ctx, cluster))

	var pdb policyv1.PodDisruptionBudget
	err := c.Get(ctx, types.NamespacedName{Name: "test-pdb", Namespace: "default"}, &pdb)
	require.NoError(t, err)
	assert.Equal(t, 4, pdb.Spec.MinAvailable.IntValue()) // max(1, 5-1) = 4
}

func TestReconcileServices_Idempotent(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	r, _ := newReconciler(cluster)
	ctx := context.Background()

	// Call twice, should not error.
	require.NoError(t, r.reconcileServices(ctx, cluster))
	require.NoError(t, r.reconcileServices(ctx, cluster))
}

func TestCreatePod_Idempotent(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
			Namespace: "default",
		},
	}
	r, _ := newReconciler(cluster, pvc)
	ctx := context.Background()

	// Create twice, should not error (second call finds existing).
	require.NoError(t, r.createPod(ctx, cluster, "test-0", 0, redisv1.LabelRolePrimary))
	require.NoError(t, r.createPod(ctx, cluster, "test-0", 0, redisv1.LabelRolePrimary))
}

// Verify list filtering works correctly.
func TestListClusterPods_Filtering(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)

	// Pod belonging to this cluster.
	myPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
	}
	// Pod belonging to a different cluster.
	otherPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "other"},
		},
	}

	r, _ := newReconciler(cluster, myPod, otherPod)
	ctx := context.Background()

	pods, err := r.listClusterPods(ctx, cluster)
	require.NoError(t, err)
	assert.Len(t, pods, 1)
	assert.Equal(t, "test-0", pods[0].Name)
}

func TestListClusterPVCs_Filtering(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)

	myPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-data-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "test"},
		},
	}
	otherPVC := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "other-data-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "other"},
		},
	}

	r, _ := newReconciler(cluster, myPVC, otherPVC)
	ctx := context.Background()

	pvcs, err := r.listClusterPVCs(ctx, cluster)
	require.NoError(t, err)
	assert.Len(t, pvcs, 1)
	assert.Equal(t, "test-data-0", pvcs[0].Name)
}

func TestReconcile_SentinelModeContinuesWhenRollingUpdateStops(t *testing.T) {
	ctx := context.Background()
	cluster := newTestCluster("test", "default", 3)
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	cluster.Spec.PrimaryUpdateStrategy = redisv1.PrimaryUpdateStrategySupervised
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "test-auth"}
	cluster.Status.CurrentPrimary = "test-0"

	r, c := newReconciler(cluster)
	desiredHash := r.computeSpecHash(cluster)

	dataPods := []corev1.Pod{
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-0",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster:  "test",
					redisv1.LabelInstance: "test-0",
					redisv1.LabelRole:     redisv1.LabelRolePrimary,
					specHashAnnotation:    "old-hash",
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-1",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster:  "test",
					redisv1.LabelInstance: "test-1",
					redisv1.LabelRole:     redisv1.LabelRoleReplica,
					specHashAnnotation:    desiredHash,
				},
			},
		},
		{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-2",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster:  "test",
					redisv1.LabelInstance: "test-2",
					redisv1.LabelRole:     redisv1.LabelRoleReplica,
					specHashAnnotation:    desiredHash,
				},
			},
		},
	}
	for i := range dataPods {
		require.NoError(t, c.Create(ctx, &dataPods[i]))
	}

	result, err := r.reconcile(ctx, cluster)
	require.NoError(t, err)
	assert.Zero(t, result.RequeueAfter)

	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.ClusterPhaseWaitingForUser, updated.Status.Phase)

	var sentinelPods corev1.PodList
	err = c.List(
		ctx,
		&sentinelPods,
		client.InNamespace("default"),
		client.MatchingLabels{
			redisv1.LabelCluster: "test",
			redisv1.LabelRole:    redisv1.LabelRoleSentinel,
		},
	)
	require.NoError(t, err)
	assert.Len(t, sentinelPods.Items, redisv1.SentinelInstances)
}

func TestReconcileReplicaModePromotion_FinalizesPromotion(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Promote: true,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: 6379,
		},
	}

	r, c := newReconciler(cluster)
	ctx := context.Background()
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
		"test-1": {Role: "slave", Connected: true, MasterLinkStatus: "up"},
	}

	promoted, err := r.reconcileReplicaModePromotion(ctx, cluster, statuses)
	require.NoError(t, err)
	assert.True(t, promoted)

	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)
	require.NotNil(t, updated.Spec.ReplicaMode)
	assert.False(t, updated.Spec.ReplicaMode.Enabled)
	assert.False(t, updated.Spec.ReplicaMode.Promote)

	var promotedCondition *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == redisv1.ConditionReplicaMode {
			promotedCondition = &updated.Status.Conditions[i]
			break
		}
	}
	require.NotNil(t, promotedCondition)
	assert.Equal(t, metav1.ConditionFalse, promotedCondition.Status)
	assert.Equal(t, "ReplicaClusterPromoted", promotedCondition.Reason)
}

func TestReconcileReplicaModePromotion_WaitsForLocalLeaderPromotion(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Promote: true,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: 6379,
		},
	}

	r, c := newReconciler(cluster)
	ctx := context.Background()
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "slave", Connected: true, MasterLinkStatus: "up"},
		"test-1": {Role: "slave", Connected: true, MasterLinkStatus: "up"},
	}

	promoted, err := r.reconcileReplicaModePromotion(ctx, cluster, statuses)
	require.NoError(t, err)
	assert.False(t, promoted)

	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)
	require.NotNil(t, updated.Spec.ReplicaMode)
	assert.True(t, updated.Spec.ReplicaMode.Enabled)
	assert.True(t, updated.Spec.ReplicaMode.Promote)
}

func TestReconcileReplicaModePromotion_FinalizesAfterSpecAlreadyDisabled(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: false,
		Promote: false,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: 6379,
		},
	}

	r, c := newReconciler(cluster)
	ctx := context.Background()
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
	}

	promoted, err := r.reconcileReplicaModePromotion(ctx, cluster, statuses)
	require.NoError(t, err)
	assert.True(t, promoted)

	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)
	require.NotNil(t, updated.Spec.ReplicaMode)
	assert.False(t, updated.Spec.ReplicaMode.Enabled)
	assert.False(t, updated.Spec.ReplicaMode.Promote)

	var promotedCondition *metav1.Condition
	for i := range updated.Status.Conditions {
		if updated.Status.Conditions[i].Type == redisv1.ConditionReplicaMode {
			promotedCondition = &updated.Status.Conditions[i]
			break
		}
	}
	require.NotNil(t, promotedCondition)
	assert.Equal(t, metav1.ConditionFalse, promotedCondition.Status)
	assert.Equal(t, "ReplicaClusterPromoted", promotedCondition.Reason)
}
