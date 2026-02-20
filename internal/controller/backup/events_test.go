package backup

import (
	"context"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
	"time"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)



// expectNoEvent asserts that no events remain on the recorder's channel.
func expectNoEvent(t *testing.T, recorder *record.FakeRecorder) {
	t.Helper()
	select {
	case event := <-recorder.Events:
		t.Fatalf("expected no event but got: %s", event)
	default:
		// good
	}
}

// drainEvents reads all pending events from the recorder and returns them.
func drainEvents(recorder *record.FakeRecorder) []string {
	var events []string
	for {
		select {
		case event := <-recorder.Events:
			events = append(events, event)
		default:
			return events
		}
	}
}

// assertContainsEvent checks that at least one event in the list contains the given type, reason, and message.
func assertContainsEvent(t *testing.T, events []string, eventType, reason, messagePart string) {
	t.Helper()
	for _, e := range events {
		if containsStr(e, eventType) && containsStr(e, reason) && containsStr(e, messagePart) {
			return
		}
	}
	t.Errorf("expected event with type=%q reason=%q message containing %q, got events:\n%v", eventType, reason, messagePart, events)
}

func containsStr(s, substr string) bool {
	for i := 0; i <= len(s)-len(substr); i++ {
		if s[i:i+len(substr)] == substr {
			return true
		}
	}
	return false
}

// --- BackupReconciler events ---

func TestReconcile_ClusterNotFound_EmitsBackupFailedEvent(t *testing.T) {
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup1", Namespace: "default"},
		Spec:       redisv1.RedisBackupSpec{ClusterName: "missing-cluster"},
	}

	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisBackup{}, &redisv1.RedisScheduledBackup{}).
		WithObjects(backup).
		Build()
	rec := record.NewFakeRecorder(100)
	r := NewBackupReconciler(c, scheme, rec)

	ctx := context.Background()
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "backup1", Namespace: "default"},
	})
	require.NoError(t, err) // setBackupFailed returns nil

	events := drainEvents(rec)
	assertContainsEvent(t, events, "Warning", "BackupFailed", "missing-cluster not found")
}

func TestReconcile_SuccessfulBackup_EmitsStartedAndCompletedEvents(t *testing.T) {
	podIP, cleanup := overrideBackupPort(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer cleanup()

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "default"},
		Status: redisv1.RedisClusterStatus{
			Phase:          redisv1.ClusterPhaseHealthy,
			CurrentPrimary: "cluster-0",
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-0", Namespace: "default",
			Labels: map[string]string{redisv1.LabelCluster: "cluster"},
		},
		Status: corev1.PodStatus{PodIP: podIP},
	}
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup1", Namespace: "default"},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "cluster",
			Target:      redisv1.BackupTargetPrimary,
		},
	}

	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisBackup{}, &redisv1.RedisScheduledBackup{}).
		WithObjects(cluster, pod, backup).
		Build()
	rec := record.NewFakeRecorder(100)
	r := NewBackupReconciler(c, scheme, rec)

	ctx := context.Background()
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "backup1", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)

	events := drainEvents(rec)
	assertContainsEvent(t, events, "Normal", "BackupStarted", "Backup started on pod cluster-0")
	assertContainsEvent(t, events, "Normal", "BackupCompleted", "Backup completed successfully")
}

func TestReconcile_BackupHTTPFailure_EmitsStartedAndFailedEvents(t *testing.T) {
	podIP, cleanup := overrideBackupPort(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer cleanup()

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "default"},
		Status: redisv1.RedisClusterStatus{
			Phase:          redisv1.ClusterPhaseHealthy,
			CurrentPrimary: "cluster-0",
		},
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "cluster-0", Namespace: "default",
			Labels: map[string]string{redisv1.LabelCluster: "cluster"},
		},
		Status: corev1.PodStatus{PodIP: podIP},
	}
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "backup1", Namespace: "default"},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "cluster",
			Target:      redisv1.BackupTargetPrimary,
		},
	}

	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisBackup{}, &redisv1.RedisScheduledBackup{}).
		WithObjects(cluster, pod, backup).
		Build()
	rec := record.NewFakeRecorder(100)
	r := NewBackupReconciler(c, scheme, rec)

	ctx := context.Background()
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "backup1", Namespace: "default"},
	})
	require.NoError(t, err) // setBackupFailed returns nil

	events := drainEvents(rec)
	assertContainsEvent(t, events, "Normal", "BackupStarted", "Backup started on pod cluster-0")
	assertContainsEvent(t, events, "Warning", "BackupFailed", "Backup failed")
}

func TestReconcile_CompletedBackup_NoEvents(t *testing.T) {
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "done", Namespace: "default"},
		Spec:       redisv1.RedisBackupSpec{ClusterName: "cluster"},
		Status:     redisv1.RedisBackupStatus{Phase: redisv1.BackupPhaseCompleted},
	}

	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisBackup{}).
		WithObjects(backup).
		Build()
	rec := record.NewFakeRecorder(100)
	r := NewBackupReconciler(c, scheme, rec)

	ctx := context.Background()
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "done", Namespace: "default"},
	})
	require.NoError(t, err)

	expectNoEvent(t, rec)
}

func TestReconcile_FailedBackup_NoEvents(t *testing.T) {
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "failed", Namespace: "default"},
		Spec:       redisv1.RedisBackupSpec{ClusterName: "cluster"},
		Status:     redisv1.RedisBackupStatus{Phase: redisv1.BackupPhaseFailed},
	}

	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisBackup{}).
		WithObjects(backup).
		Build()
	rec := record.NewFakeRecorder(100)
	r := NewBackupReconciler(c, scheme, rec)

	ctx := context.Background()
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "failed", Namespace: "default"},
	})
	require.NoError(t, err)

	expectNoEvent(t, rec)
}

// --- ScheduledBackupReconciler events ---

func TestScheduledReconcile_CreatesBackup_EmitsScheduledBackupTriggeredEvent(t *testing.T) {
	lastSchedule := metav1.NewTime(time.Now().Add(-2 * time.Hour))

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "default"},
	}

	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "daily", Namespace: "default"},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 * * * *",
			ClusterName: "cluster",
			Target:      redisv1.BackupTargetPrimary,
		},
		Status: redisv1.RedisScheduledBackupStatus{
			LastScheduleTime: &lastSchedule,
		},
	}

	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisBackup{}, &redisv1.RedisScheduledBackup{}).
		WithObjects(cluster, scheduled).
		Build()
	rec := record.NewFakeRecorder(100)
	r := NewScheduledBackupReconciler(c, scheme, rec)

	ctx := context.Background()
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, requeueInterval, result.RequeueAfter)

	events := drainEvents(rec)
	assertContainsEvent(t, events, "Normal", "ScheduledBackupTriggered", "Created backup")
}

func TestScheduledReconcile_NotYetDue_NoEvent(t *testing.T) {
	lastSchedule := metav1.NewTime(time.Now().Add(-10 * time.Minute))

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster", Namespace: "default"},
	}

	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "daily", Namespace: "default"},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 * * * *",
			ClusterName: "cluster",
		},
		Status: redisv1.RedisScheduledBackupStatus{
			LastScheduleTime: &lastSchedule,
		},
	}

	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisBackup{}, &redisv1.RedisScheduledBackup{}).
		WithObjects(cluster, scheduled).
		Build()
	rec := record.NewFakeRecorder(100)
	r := NewScheduledBackupReconciler(c, scheme, rec)

	ctx := context.Background()
	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// Not yet due -- no ScheduledBackupTriggered event.
	expectNoEvent(t, rec)
}

func TestScheduledReconcile_Suspended_NoEvent(t *testing.T) {
	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "daily", Namespace: "default"},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 0 * * *",
			ClusterName: "cluster",
			Suspend:     boolPtr(true),
		},
	}

	scheme := testScheme()
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisScheduledBackup{}).
		WithObjects(scheduled).
		Build()
	rec := record.NewFakeRecorder(100)
	r := NewScheduledBackupReconciler(c, scheme, rec)

	ctx := context.Background()
	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	require.NoError(t, err)

	expectNoEvent(t, rec)
}
