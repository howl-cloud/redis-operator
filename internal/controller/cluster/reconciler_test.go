package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
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
			Mode:                     redisv1.ClusterModeStandalone,
			ImageName:                "redis:7.2",
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
	return NewClusterReconciler(c, scheme, recorder), c
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

	err := r.reconcilePVCs(ctx, cluster)
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
}

func TestReconcileServiceAccount_Idempotent(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	r, _ := newReconciler(cluster)
	ctx := context.Background()

	// Call twice.
	require.NoError(t, r.reconcileServiceAccount(ctx, cluster))
	require.NoError(t, r.reconcileServiceAccount(ctx, cluster))
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

	err := r.reconcilePVCs(ctx, cluster)
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
				"my-auth":   "100",
				"my-tls":    "200",
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
	for _, vol := range pod.Spec.Volumes {
		if vol.Name == projectedVolumeName {
			foundProjected = true
			require.NotNil(t, vol.Projected)
			assert.Len(t, vol.Projected.Sources, 2) // auth + tls
		}
	}
	assert.True(t, foundProjected, "projected volume should be present")

	// Verify mount.
	foundMount := false
	for _, mount := range pod.Spec.Containers[0].VolumeMounts {
		if mount.Name == projectedVolumeName {
			foundMount = true
			assert.Equal(t, projectedMountPath, mount.MountPath)
		}
	}
	assert.True(t, foundMount, "projected volume mount should be present")
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
	assert.Equal(t, "/readyz", container.ReadinessProbe.HTTPGet.Path)

	// Verify env vars.
	envMap := make(map[string]string)
	for _, e := range container.Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, "test-0", envMap["POD_NAME"])
	assert.Equal(t, "test", envMap["CLUSTER_NAME"])
	assert.Equal(t, "default", envMap["POD_NAMESPACE"])

	// Verify init container.
	require.Len(t, pod.Spec.InitContainers, 1)
	assert.Equal(t, "copy-manager", pod.Spec.InitContainers[0].Name)
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

