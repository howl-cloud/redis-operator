package backup

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func testScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(s))
	utilruntime.Must(redisv1.AddToScheme(s))
	return s
}

func newBackupReconciler(objs ...client.Object) (*BackupReconciler, client.Client) {
	scheme := testScheme()
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisBackup{}, &redisv1.RedisScheduledBackup{})
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	c := builder.Build()
	recorder := record.NewFakeRecorder(100)
	return NewBackupReconciler(c, scheme, recorder), c
}

func TestSelectTargetPod_PreferReplica(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Status: redisv1.RedisClusterStatus{
			CurrentPrimary: "cluster-0",
		},
	}

	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-0",
				Namespace: "default",
				Labels:    map[string]string{redisv1.LabelCluster: "cluster"},
			},
			Status: corev1.PodStatus{PodIP: "10.0.0.1"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-1",
				Namespace: "default",
				Labels:    map[string]string{redisv1.LabelCluster: "cluster"},
			},
			Status: corev1.PodStatus{PodIP: "10.0.0.2"},
		},
	}

	r, _ := newBackupReconciler(append([]client.Object{cluster}, pods...)...)
	ctx := context.Background()

	name, ip, err := r.selectTargetPod(ctx, cluster, redisv1.BackupTargetPreferReplica)
	require.NoError(t, err)
	assert.Equal(t, "cluster-1", name)
	assert.Equal(t, "10.0.0.2", ip)
}

func TestSelectTargetPod_FallbackToPrimary(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Status: redisv1.RedisClusterStatus{
			CurrentPrimary: "cluster-0",
		},
	}

	// Only primary pod exists.
	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-0",
				Namespace: "default",
				Labels:    map[string]string{redisv1.LabelCluster: "cluster"},
			},
			Status: corev1.PodStatus{PodIP: "10.0.0.1"},
		},
	}

	r, _ := newBackupReconciler(append([]client.Object{cluster}, pods...)...)
	ctx := context.Background()

	name, ip, err := r.selectTargetPod(ctx, cluster, redisv1.BackupTargetPreferReplica)
	require.NoError(t, err)
	assert.Equal(t, "cluster-0", name)
	assert.Equal(t, "10.0.0.1", ip)
}

func TestSelectTargetPod_PrimaryTarget(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Status: redisv1.RedisClusterStatus{
			CurrentPrimary: "cluster-0",
		},
	}

	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-0",
				Namespace: "default",
				Labels:    map[string]string{redisv1.LabelCluster: "cluster"},
			},
			Status: corev1.PodStatus{PodIP: "10.0.0.1"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "cluster-1",
				Namespace: "default",
				Labels:    map[string]string{redisv1.LabelCluster: "cluster"},
			},
			Status: corev1.PodStatus{PodIP: "10.0.0.2"},
		},
	}

	r, _ := newBackupReconciler(append([]client.Object{cluster}, pods...)...)
	ctx := context.Background()

	name, ip, err := r.selectTargetPod(ctx, cluster, redisv1.BackupTargetPrimary)
	require.NoError(t, err)
	// With BackupTargetPrimary, it should skip the prefer-replica path and
	// fall through to the primary fallback.
	assert.Equal(t, "cluster-0", name)
	assert.Equal(t, "10.0.0.1", ip)
}

func TestSelectTargetPod_NoPods(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Status: redisv1.RedisClusterStatus{
			CurrentPrimary: "cluster-0",
		},
	}

	r, _ := newBackupReconciler(cluster)
	ctx := context.Background()

	_, _, err := r.selectTargetPod(ctx, cluster, redisv1.BackupTargetPreferReplica)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no suitable pod found")
}

func TestSetBackupFailed(t *testing.T) {
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-backup",
			Namespace: "default",
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "cluster",
		},
	}

	r, c := newBackupReconciler(backup)
	ctx := context.Background()

	err := r.setBackupFailed(ctx, backup, "something went wrong")
	require.NoError(t, err)

	// Re-fetch.
	var updated redisv1.RedisBackup
	err = c.Get(ctx, types.NamespacedName{Name: "test-backup", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.BackupPhaseFailed, updated.Status.Phase)
	assert.Equal(t, "something went wrong", updated.Status.Error)
}

func TestSetBackupCompleted(t *testing.T) {
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-backup",
			Namespace: "default",
		},
		Status: redisv1.RedisBackupStatus{
			Phase: redisv1.BackupPhaseRunning,
		},
	}

	r, c := newBackupReconciler(backup)
	ctx := context.Background()

	err := r.setBackupCompleted(ctx, backup)
	require.NoError(t, err)

	var updated redisv1.RedisBackup
	err = c.Get(ctx, types.NamespacedName{Name: "test-backup", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.BackupPhaseCompleted, updated.Status.Phase)
	assert.NotNil(t, updated.Status.CompletedAt)
}

func TestStartBackup_UpdatesStatus(t *testing.T) {
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-backup",
			Namespace: "default",
		},
	}

	r, c := newBackupReconciler(backup)
	ctx := context.Background()

	// We can't test the actual HTTP call, but we can test the status update part
	// by calling startBackup with a mock HTTP server.
	// For now, test only the status transition logic via setBackupFailed/setBackupCompleted.
	// The HTTP call is tested separately.

	// Test that phase transitions are recorded.
	err := r.setBackupFailed(ctx, backup, "test error")
	require.NoError(t, err)

	var updated redisv1.RedisBackup
	err = c.Get(ctx, types.NamespacedName{Name: "test-backup", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.BackupPhaseFailed, updated.Status.Phase)
}

// overrideBackupPort starts an httptest.Server and overrides instanceManagerPort
// so triggerBackup connects to the test server. Returns the IP to use as podIP
// and a cleanup function.
func overrideBackupPort(t *testing.T, handler http.Handler) (string, func()) {
	t.Helper()
	server := httptest.NewServer(handler)
	_, portStr, err := net.SplitHostPort(server.Listener.Addr().String())
	require.NoError(t, err)
	port, err := strconv.Atoi(portStr)
	require.NoError(t, err)

	oldPort := instanceManagerPort
	instanceManagerPort = port
	return "127.0.0.1", func() {
		instanceManagerPort = oldPort
		server.Close()
	}
}

func TestReconcile_BackupNotFound(t *testing.T) {
	r, _ := newBackupReconciler()
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)
}

func TestReconcile_BackupAlreadyCompleted(t *testing.T) {
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "done-backup",
			Namespace: "default",
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "cluster",
		},
		Status: redisv1.RedisBackupStatus{
			Phase: redisv1.BackupPhaseCompleted,
		},
	}

	r, _ := newBackupReconciler(backup)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "done-backup", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)
}

func TestReconcile_BackupAlreadyFailed(t *testing.T) {
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "failed-backup",
			Namespace: "default",
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "cluster",
		},
		Status: redisv1.RedisBackupStatus{
			Phase: redisv1.BackupPhaseFailed,
		},
	}

	r, _ := newBackupReconciler(backup)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "failed-backup", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)
}

func TestReconcile_ClusterNotFound(t *testing.T) {
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup1",
			Namespace: "default",
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "missing-cluster",
		},
	}

	r, c := newBackupReconciler(backup)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "backup1", Namespace: "default"},
	})
	// setBackupFailed returns the status patch error (nil), but the reconcile
	// returns the setBackupFailed error.
	require.NoError(t, err)

	// Verify backup was marked failed.
	var updated redisv1.RedisBackup
	err = c.Get(ctx, types.NamespacedName{Name: "backup1", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.BackupPhaseFailed, updated.Status.Phase)
	assert.Contains(t, updated.Status.Error, "cluster missing-cluster not found")
}

func TestReconcile_ClusterNotHealthy(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Status: redisv1.RedisClusterStatus{
			Phase: redisv1.ClusterPhaseCreating,
		},
	}

	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup1",
			Namespace: "default",
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "cluster",
		},
	}

	r, _ := newBackupReconciler(cluster, backup)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "backup1", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, requeueInterval, result.RequeueAfter)
}

func TestReconcile_SuccessfulBackup(t *testing.T) {
	// Start a mock instance manager that accepts the backup request.
	podIP, cleanup := overrideBackupPort(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer cleanup()

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Status: redisv1.RedisClusterStatus{
			Phase:          redisv1.ClusterPhaseHealthy,
			CurrentPrimary: "cluster-0",
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "cluster"},
		},
		Status: corev1.PodStatus{PodIP: podIP},
	}

	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup1",
			Namespace: "default",
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "cluster",
			Target:      redisv1.BackupTargetPrimary,
		},
	}

	r, c := newBackupReconciler(cluster, pod, backup)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "backup1", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)

	// Verify backup completed.
	var updated redisv1.RedisBackup
	err = c.Get(ctx, types.NamespacedName{Name: "backup1", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.BackupPhaseCompleted, updated.Status.Phase)
	assert.NotNil(t, updated.Status.CompletedAt)
}

func TestReconcile_BackupHTTPFailure(t *testing.T) {
	// Start a mock instance manager that returns an error.
	podIP, cleanup := overrideBackupPort(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cleanup()

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
		Status: redisv1.RedisClusterStatus{
			Phase:          redisv1.ClusterPhaseHealthy,
			CurrentPrimary: "cluster-0",
		},
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-0",
			Namespace: "default",
			Labels:    map[string]string{redisv1.LabelCluster: "cluster"},
		},
		Status: corev1.PodStatus{PodIP: podIP},
	}

	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "backup1",
			Namespace: "default",
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "cluster",
			Target:      redisv1.BackupTargetPrimary,
		},
	}

	r, c := newBackupReconciler(cluster, pod, backup)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "backup1", Namespace: "default"},
	})
	// setBackupFailed returns nil, so reconcile returns nil.
	require.NoError(t, err)

	// Verify backup was marked failed.
	var updated redisv1.RedisBackup
	err = c.Get(ctx, types.NamespacedName{Name: "backup1", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.BackupPhaseFailed, updated.Status.Phase)
	assert.Contains(t, updated.Status.Error, "starting backup")
}

// Suppress unused import warning.
var _ = time.Second
