package reconciler

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

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

// --- InstanceFenced event ---

func TestReconcile_Fenced_EmitsInstanceFencedEvent(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Annotations = map[string]string{
		redisv1.FencingAnnotationKey: `["test-0"]`,
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := record.NewFakeRecorder(100)
	r := NewInstanceReconciler(fakeClient, redisClient, rec, "test", "test-0", "default")

	result, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)

	events := drainEvents(rec)
	assertContainsEvent(t, events, "Warning", "InstanceFenced", "test-0 is fenced")
}

func TestReconcile_NotFenced_NoInstanceFencedEvent(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"
	// No fencing annotation.

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := record.NewFakeRecorder(100)
	r := NewInstanceReconciler(fakeClient, redisClient, rec, "test", "test-0", "default")

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.NoError(t, err)

	events := drainEvents(rec)
	for _, e := range events {
		assert.NotContains(t, e, "InstanceFenced")
	}
}

// --- ConfigReloaded event ---

func TestReconcile_WithConfig_EmitsConfigReloadedEvent(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Spec.Redis = map[string]string{
		"maxmemory": "256mb",
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := record.NewFakeRecorder(100)
	r := NewInstanceReconciler(fakeClient, redisClient, rec, "test", "test-0", "default")

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.NoError(t, err)

	events := drainEvents(rec)
	assertContainsEvent(t, events, "Normal", "ConfigReloaded", "Redis configuration reloaded")
}

func TestReconcile_NoConfig_NoConfigReloadedEvent(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"
	// No Redis config.

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := record.NewFakeRecorder(100)
	r := NewInstanceReconciler(fakeClient, redisClient, rec, "test", "test-0", "default")

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.NoError(t, err)

	events := drainEvents(rec)
	for _, e := range events {
		assert.NotContains(t, e, "ConfigReloaded")
	}
}

// --- PromotedToPrimary event ---

func TestReconcileRole_PromotedToPrimary_EmitsEvent(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "slave" // Currently slave, should be primary.
	srv.mu.Unlock()

	rec := record.NewFakeRecorder(100)
	r := &InstanceReconciler{
		redisClient: redisClient,
		recorder:    rec,
		podName:     "test-0",
		namespace:   "default",
	}

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0" // This pod should be primary.

	err := r.reconcileRole(context.Background(), cluster)
	require.NoError(t, err)

	events := drainEvents(rec)
	assertContainsEvent(t, events, "Normal", "PromotedToPrimary", "test-0 promoted to primary")
}

func TestReconcileRole_AlreadyPrimary_NoEvent(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "master" // Already correct.
	srv.mu.Unlock()

	rec := record.NewFakeRecorder(100)
	r := &InstanceReconciler{
		redisClient: redisClient,
		recorder:    rec,
		podName:     "test-0",
		namespace:   "default",
	}

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"

	err := r.reconcileRole(context.Background(), cluster)
	require.NoError(t, err)

	expectNoEvent(t, rec)
}

// --- DemotedToReplica event ---

func TestReconcileRole_DemotedToReplica_EmitsEvent(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "master" // Currently master, should be replica.
	srv.mu.Unlock()

	rec := record.NewFakeRecorder(100)
	r := &InstanceReconciler{
		redisClient: redisClient,
		recorder:    rec,
		podName:     "test-1", // Not the primary.
		namespace:   "default",
	}

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0" // test-1 should be replica.

	err := r.reconcileRole(context.Background(), cluster)
	require.NoError(t, err)

	events := drainEvents(rec)
	assertContainsEvent(t, events, "Normal", "DemotedToReplica", "test-1 demoted to replica of test-0")
}

func TestReconcileRole_AlreadyReplica_NoEvent(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "slave" // Already correct.
	srv.mu.Unlock()

	rec := record.NewFakeRecorder(100)
	r := &InstanceReconciler{
		redisClient: redisClient,
		recorder:    rec,
		podName:     "test-1",
		namespace:   "default",
	}

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"

	err := r.reconcileRole(context.Background(), cluster)
	require.NoError(t, err)

	expectNoEvent(t, rec)
}

// --- Full reconcile event combinations ---

func TestReconcile_FencedPod_OnlyEmitsInstanceFencedEvent(t *testing.T) {
	_, redisClient := newFakeRedisServer(t)

	cluster := newTestCluster("test", "default")
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Annotations = map[string]string{
		redisv1.FencingAnnotationKey: `["test-0"]`,
	}
	cluster.Spec.Redis = map[string]string{"maxmemory": "256mb"}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := record.NewFakeRecorder(100)
	r := NewInstanceReconciler(fakeClient, redisClient, rec, "test", "test-0", "default")

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.NoError(t, err)

	events := drainEvents(rec)
	// Only InstanceFenced should be emitted; config and role events should NOT appear
	// because the reconciler returns early on fencing.
	assertContainsEvent(t, events, "Warning", "InstanceFenced", "test-0 is fenced")
	for _, e := range events {
		assert.NotContains(t, e, "ConfigReloaded")
		assert.NotContains(t, e, "PromotedToPrimary")
		assert.NotContains(t, e, "DemotedToReplica")
	}
}

func TestReconcile_PromoteAndConfig_EmitsBothEvents(t *testing.T) {
	srv, redisClient := newFakeRedisServer(t)
	srv.mu.Lock()
	srv.role = "slave" // Should be promoted.
	srv.mu.Unlock()

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "test", Namespace: "default"},
		Spec: redisv1.RedisClusterSpec{
			Instances: 3,
			ImageName: "redis:7.2",
			Redis: map[string]string{
				"maxmemory": "512mb",
			},
		},
		Status: redisv1.RedisClusterStatus{
			CurrentPrimary: "test-0",
		},
	}

	scheme := testScheme()
	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(cluster).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	rec := record.NewFakeRecorder(100)
	r := NewInstanceReconciler(fakeClient, redisClient, rec, "test", "test-0", "default")

	_, err := r.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "test", Namespace: "default"},
	})
	require.NoError(t, err)

	events := drainEvents(rec)
	assertContainsEvent(t, events, "Normal", "PromotedToPrimary", "test-0 promoted to primary")
	assertContainsEvent(t, events, "Normal", "ConfigReloaded", "Redis configuration reloaded")
}
