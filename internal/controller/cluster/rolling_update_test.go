package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestRollingUpdate_NoOutdatedPods(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-0"

	desiredHash := "abc123"
	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-0",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster:  "test",
					redisv1.LabelInstance: "test-0",
					"redis.io/spec-hash": desiredHash,
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-1",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster:  "test",
					redisv1.LabelInstance: "test-1",
					"redis.io/spec-hash": desiredHash,
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-2",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster:  "test",
					redisv1.LabelInstance: "test-2",
					"redis.io/spec-hash": desiredHash,
				},
			},
		},
	}

	r, c := newReconciler(append([]client.Object{cluster}, pods...)...)
	ctx := context.Background()

	err := r.rollingUpdate(ctx, cluster, desiredHash)
	require.NoError(t, err)

	// All pods should still exist (no deletions).
	for _, name := range []string{"test-0", "test-1", "test-2"} {
		var pod corev1.Pod
		err := c.Get(ctx, types.NamespacedName{Name: name, Namespace: "default"}, &pod)
		assert.NoError(t, err, "pod %s should still exist", name)
	}
}

func TestRollingUpdate_ReplicaOutdated(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-0"

	desiredHash := "new-hash"
	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-0",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster: "test",
					"redis.io/spec-hash": desiredHash,
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-1",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster: "test",
					"redis.io/spec-hash": "old-hash",
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-2",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster: "test",
					"redis.io/spec-hash": desiredHash,
				},
			},
		},
	}

	r, c := newReconciler(append([]client.Object{cluster}, pods...)...)
	ctx := context.Background()

	err := r.rollingUpdate(ctx, cluster, desiredHash)
	require.NoError(t, err)

	// test-1 (outdated replica) should be deleted.
	var deleted corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-1", Namespace: "default"}, &deleted)
	assert.Error(t, err, "test-1 should be deleted")

	// Primary should still exist.
	var primary corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-0", Namespace: "default"}, &primary)
	assert.NoError(t, err, "primary should still exist")
}

func TestRollingUpdate_HighestOrdinalFirst(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-0"

	desiredHash := "new-hash"
	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-0",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster: "test",
					"redis.io/spec-hash": desiredHash,
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-1",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster: "test",
					"redis.io/spec-hash": "old-hash",
				},
			},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-2",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster: "test",
					"redis.io/spec-hash": "old-hash",
				},
			},
		},
	}

	r, c := newReconciler(append([]client.Object{cluster}, pods...)...)
	ctx := context.Background()

	// First call: should delete highest ordinal (test-2), one at a time.
	err := r.rollingUpdate(ctx, cluster, desiredHash)
	require.NoError(t, err)

	// test-2 should be deleted (highest ordinal first).
	var pod2 corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-2", Namespace: "default"}, &pod2)
	assert.Error(t, err, "test-2 should be deleted first (highest ordinal)")

	// test-1 should still exist (one at a time).
	var pod1 corev1.Pod
	err = c.Get(ctx, types.NamespacedName{Name: "test-1", Namespace: "default"}, &pod1)
	assert.NoError(t, err, "test-1 should still exist (not yet updated)")
}

func TestRollingUpdate_OnlyPrimaryOutdated(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Status.InstancesStatus = map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: false},
		"test-1": {Role: "slave", Connected: true, ReplicationOffset: 9000},
	}

	desiredHash := "new-hash"
	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-0",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster: "test",
					"redis.io/spec-hash": "old-hash",
				},
			},
			Status: corev1.PodStatus{PodIP: "10.0.0.1"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-1",
				Namespace: "default",
				Labels: map[string]string{
					redisv1.LabelCluster: "test",
					"redis.io/spec-hash": desiredHash,
				},
			},
			Status: corev1.PodStatus{PodIP: "10.0.0.2"},
		},
	}

	r, _ := newReconciler(append([]client.Object{cluster}, pods...)...)
	ctx := context.Background()

	// This should attempt switchover, which will try to promote test-1 via HTTP.
	// The HTTP call will fail (no server), giving us an error that includes "promoting".
	err := r.rollingUpdate(ctx, cluster, desiredHash)
	// Switchover will fail because promoteInstance makes an HTTP call to a pod IP
	// that isn't running. This is expected -- we verify the switchover path was taken.
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "switchover during rolling update")
}

func TestRollingUpdate_AllUpToDate(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Status.CurrentPrimary = "test-0"

	desiredHash := "current"
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "test",
				"redis.io/spec-hash": desiredHash,
			},
		},
	}

	r, _ := newReconciler(cluster, pod)
	ctx := context.Background()

	err := r.rollingUpdate(ctx, cluster, desiredHash)
	require.NoError(t, err)
}

func TestSwitchover_NoCandidates(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"
	// No InstancesStatus -- selectFailoverCandidate returns ""
	cluster.Status.InstancesStatus = map[string]redisv1.InstanceStatus{}

	r, _ := newReconciler(cluster)
	ctx := context.Background()

	err := r.switchover(ctx, cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "no suitable replica found for switchover")
}

func TestSwitchover_CandidateFound(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)
	cluster.Status.CurrentPrimary = "test-0"
	cluster.Status.InstancesStatus = map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: false},
		"test-1": {Role: "slave", Connected: true, ReplicationOffset: 9500},
	}

	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-0",
				Namespace: "default",
				Labels:    map[string]string{redisv1.LabelCluster: "test"},
			},
			Status: corev1.PodStatus{PodIP: "10.0.0.1"},
		},
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-1",
				Namespace: "default",
				Labels:    map[string]string{redisv1.LabelCluster: "test"},
			},
			Status: corev1.PodStatus{PodIP: "10.0.0.2"},
		},
	}

	r, _ := newReconciler(append([]client.Object{cluster}, pods...)...)
	ctx := context.Background()

	// switchover will try to promote test-1 via HTTP.
	// It will fail because there's no server at 10.0.0.2:8080.
	// We verify the right candidate was selected by checking the error message.
	err := r.switchover(ctx, cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "promoting test-1")
}
