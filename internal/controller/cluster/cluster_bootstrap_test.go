package cluster

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestReconcileClusterBootstrap_DoesNotBlockWhenPodsAreNotCreatedYet(t *testing.T) {
	cluster := newTestCluster("test", "default", 0)
	cluster.Spec.Mode = redisv1.ClusterModeCluster
	cluster.Spec.Shards = 3
	cluster.Spec.ReplicasPerShard = 0

	r, _ := newReconciler(cluster)

	ready, err := r.reconcileClusterBootstrap(context.Background(), cluster, map[string]redisv1.InstanceStatus{})
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestReconcileClusterBootstrap_DoesNotBlockWhenPodStatusesAreMissing(t *testing.T) {
	cluster := newTestCluster("test", "default", 0)
	cluster.Spec.Mode = redisv1.ClusterModeCluster
	cluster.Spec.Shards = 3
	cluster.Spec.ReplicasPerShard = 0

	pod0 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-0",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "test",
			},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.10"},
	}
	pod1 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-1",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "test",
			},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.11"},
	}
	pod2 := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-2",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "test",
			},
		},
		Status: corev1.PodStatus{PodIP: "10.0.0.12"},
	}

	r, _ := newReconciler(cluster, pod0, pod1, pod2)

	ready, err := r.reconcileClusterBootstrap(context.Background(), cluster, map[string]redisv1.InstanceStatus{})
	require.NoError(t, err)
	assert.True(t, ready)
}

func TestCalculateUncoveredSlotRanges_ReturnsMissingSegments(t *testing.T) {
	coverage := calculateSlotCoverage(map[string]redisv1.InstanceStatus{
		"node-a": {
			SlotsServed: []redisv1.SlotRange{
				{Start: 0, End: 100},
				{Start: 300, End: 350},
			},
		},
	})

	missing := calculateUncoveredSlotRanges(redisv1.SlotRange{Start: 0, End: 400}, &coverage)
	assert.Equal(t, []redisv1.SlotRange{
		{Start: 101, End: 299},
		{Start: 351, End: 400},
	}, missing)
}

type recordedPost struct {
	pod      string
	endpoint string
	body     map[string]any
}

func fakeInstanceManagers(t *testing.T, ipToPod map[string]string) *[]recordedPost {
	t.Helper()
	var (
		mu    sync.Mutex
		posts []recordedPost
	)
	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		posts = append(posts, recordedPost{pod: ipToPod[host], endpoint: r.URL.Path, body: body})
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	})
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{Handler: handler, ReadHeaderTimeout: time.Second}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })

	previous := instanceManagerPort
	instanceManagerPort = listener.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { instanceManagerPort = previous })
	return &posts
}

func TestReconcileClusterBootstrap_RaisingReplicasPerShardKeepsExistingPrimaries(t *testing.T) {
	cluster := newClusterModeCluster(3, 1)
	cluster.Status.ClusterState = "ok"
	cluster.Status.SlotsAssigned = 16384

	ipToPod := map[string]string{"127.0.0.1": "any"}
	var objs []client.Object
	pods := []corev1.Pod{
		shardPod("test-0", "s0", "primary"),
		shardPod("test-1", "s1", "primary"),
		shardPod("test-2", "s2", "primary"),
		shardPod("test-3", "s1", "replica"),
		shardPod("test-4", "s2", "primary"),
		shardPod("test-5", "s2", "replica"),
	}
	for i := range pods {
		pods[i].Status.PodIP = "127.0.0.1"
		objs = append(objs, &pods[i])
	}
	posts := fakeInstanceManagers(t, ipToPod)

	ranges := calculateClusterSlotRanges(3)
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": ownerStatus("n0", ranges[0]),
		"test-1": ownerStatus("n1", ranges[1]),
		"test-2": ownerStatus("n2", ranges[2]),
		"test-3": emptyPrimaryStatus("n3"),
		"test-4": emptyPrimaryStatus("n4"),
		"test-5": emptyPrimaryStatus("n5"),
	}

	r, _ := newReconciler(append([]client.Object{cluster}, objs...)...)
	ready, err := r.reconcileClusterBootstrap(context.Background(), cluster, statuses)
	require.NoError(t, err)
	assert.False(t, ready, "bootstrap acted this pass and must requeue")

	var replicateTargets []string
	for _, post := range *posts {
		switch post.endpoint {
		case "/v1/cluster/meet":
		case "/v1/cluster/replicate":
			replicateTargets = append(replicateTargets, post.body["nodeID"].(string))
		default:
			t.Fatalf("unexpected call %s: slot owners must not be touched", post.endpoint)
		}
	}
	assert.Equal(t, []string{"n0", "n1", "n2"}, replicateTargets)
}

func TestReconcileClusterBootstrap_LeavesCorrectlyAttachedReplicasAlone(t *testing.T) {
	cluster := newClusterModeCluster(3, 1)
	cluster.Status.ClusterState = "ok"
	cluster.Status.SlotsAssigned = 16384

	var objs []client.Object
	pods := []corev1.Pod{
		shardPod("test-0", "s0", "primary"), shardPod("test-1", "s1", "primary"),
		shardPod("test-2", "s2", "primary"), shardPod("test-3", "s0", "replica"),
		shardPod("test-4", "s1", "replica"), shardPod("test-5", "s2", "replica"),
	}
	for i := range pods {
		pods[i].Status.PodIP = "127.0.0.1"
		objs = append(objs, &pods[i])
	}
	posts := fakeInstanceManagers(t, map[string]string{})

	ranges := calculateClusterSlotRanges(3)
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": ownerStatus("n0", ranges[0]),
		"test-1": ownerStatus("n1", ranges[1]),
		"test-2": ownerStatus("n2", ranges[2]),
		"test-3": replicaStatus("n3", "n0"),
		"test-4": replicaStatus("n4", "n1"),
		"test-5": replicaStatus("n5", "n2"),
	}

	r, _ := newReconciler(append([]client.Object{cluster}, objs...)...)
	ready, err := r.reconcileClusterBootstrap(context.Background(), cluster, statuses)
	require.NoError(t, err)
	assert.True(t, ready)
	for _, post := range *posts {
		assert.Equal(t, "/v1/cluster/meet", post.endpoint)
	}
}

func TestReconcileClusterBootstrap_RequeuesWhenReplicaDoesNotKnowPrimaryYet(t *testing.T) {
	cluster := newClusterModeCluster(3, 1)
	var objs []client.Object
	pods := []corev1.Pod{
		shardPod("test-0", "s0", "primary"), shardPod("test-1", "s1", "primary"),
		shardPod("test-2", "s2", "primary"), shardPod("test-3", "", ""),
		shardPod("test-4", "", ""), shardPod("test-5", "", ""),
	}
	for i := range pods {
		pods[i].Status.PodIP = "127.0.0.1"
		objs = append(objs, &pods[i])
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	server := &http.Server{ReadHeaderTimeout: time.Second, Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/cluster/replicate" {
			http.Error(w, "node n0 is not known to this instance yet", http.StatusConflict)
			return
		}
		w.WriteHeader(http.StatusOK)
	})}
	go func() { _ = server.Serve(listener) }()
	t.Cleanup(func() { _ = server.Close() })
	previous := instanceManagerPort
	instanceManagerPort = listener.Addr().(*net.TCPAddr).Port
	t.Cleanup(func() { instanceManagerPort = previous })

	ranges := calculateClusterSlotRanges(3)
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": ownerStatus("n0", ranges[0]),
		"test-1": ownerStatus("n1", ranges[1]),
		"test-2": ownerStatus("n2", ranges[2]),
		"test-3": emptyPrimaryStatus("n3"),
		"test-4": emptyPrimaryStatus("n4"),
		"test-5": emptyPrimaryStatus("n5"),
	}

	r, _ := newReconciler(append([]client.Object{cluster}, objs...)...)
	ready, err := r.reconcileClusterBootstrap(context.Background(), cluster, statuses)
	require.NoError(t, err, "a 409 is a transient state, not a reconcile failure")
	assert.False(t, ready)
}

func TestClusterHTTPError_IncludesBody(t *testing.T) {
	err := &clusterHTTPError{url: "http://10.0.0.1:8080/v1/cluster/replicate", status: 500, body: "cluster replicate failed: ERR To set a master the node must be empty"}
	assert.Contains(t, err.Error(), "status 500: cluster replicate failed: ERR To set a master")
}
