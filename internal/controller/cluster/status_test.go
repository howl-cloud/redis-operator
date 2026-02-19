package cluster

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestDeterminePhase_EmptyStatuses(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{Instances: 3},
	}
	phase := determinePhase(cluster, map[string]redisv1.InstanceStatus{})
	assert.Equal(t, redisv1.ClusterPhaseCreating, phase)
}

func TestDeterminePhase_Healthy(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{Instances: 3},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
		"test-1": {Role: "slave", Connected: true},
		"test-2": {Role: "slave", Connected: true},
	}
	phase := determinePhase(cluster, statuses)
	assert.Equal(t, redisv1.ClusterPhaseHealthy, phase)
}

func TestDeterminePhase_Degraded(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{Instances: 3},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
		"test-1": {Role: "slave", Connected: true},
		"test-2": {Role: "slave", Connected: false},
	}
	phase := determinePhase(cluster, statuses)
	assert.Equal(t, redisv1.ClusterPhaseDegraded, phase)
}

func TestDeterminePhase_FailingOver(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{Instances: 3},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "slave", Connected: true},
		"test-1": {Role: "slave", Connected: true},
		"test-2": {Role: "slave", Connected: true},
	}
	phase := determinePhase(cluster, statuses)
	assert.Equal(t, redisv1.ClusterPhaseFailingOver, phase)
}

func TestDeterminePhase_FewerInstancesThanDesired(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{Instances: 3},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
		"test-1": {Role: "slave", Connected: true},
	}
	phase := determinePhase(cluster, statuses)
	assert.Equal(t, redisv1.ClusterPhaseDegraded, phase)
}

func TestDetermineConditions_WithConnectedMaster(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Status: redisv1.RedisClusterStatus{
			CurrentPrimary: "test-0",
		},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
		"test-1": {Role: "slave", Connected: true, MasterLinkStatus: "up"},
	}

	conditions := determineConditions(cluster, statuses)

	// Find the Ready condition.
	var readyCond, primaryCond, replCond *metav1.Condition
	for i := range conditions {
		switch conditions[i].Type {
		case redisv1.ConditionReady:
			readyCond = &conditions[i]
		case redisv1.ConditionPrimaryAvailable:
			primaryCond = &conditions[i]
		case redisv1.ConditionReplicationHealthy:
			replCond = &conditions[i]
		}
	}

	assert.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionTrue, readyCond.Status)
	assert.Equal(t, "ClusterReady", readyCond.Reason)

	assert.NotNil(t, primaryCond)
	assert.Equal(t, metav1.ConditionTrue, primaryCond.Status)
	assert.Equal(t, "PrimaryRunning", primaryCond.Reason)

	assert.NotNil(t, replCond)
	assert.Equal(t, metav1.ConditionTrue, replCond.Status)
	assert.Equal(t, "ReplicasConnected", replCond.Reason)
}

func TestDetermineConditions_NoPrimary(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Status: redisv1.RedisClusterStatus{
			CurrentPrimary: "test-0",
		},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "slave", Connected: true, MasterLinkStatus: "down"},
		"test-1": {Role: "slave", Connected: true, MasterLinkStatus: "down"},
	}

	conditions := determineConditions(cluster, statuses)

	var readyCond *metav1.Condition
	for i := range conditions {
		if conditions[i].Type == redisv1.ConditionReady {
			readyCond = &conditions[i]
			break
		}
	}

	assert.NotNil(t, readyCond)
	assert.Equal(t, metav1.ConditionFalse, readyCond.Status)
	assert.Equal(t, "NoPrimaryAvailable", readyCond.Reason)
}

func TestDetermineConditions_ReplicationUnhealthy(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Status: redisv1.RedisClusterStatus{
			CurrentPrimary: "test-0",
		},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
		"test-1": {Role: "slave", Connected: true, MasterLinkStatus: "down"},
	}

	conditions := determineConditions(cluster, statuses)

	var replCond *metav1.Condition
	for i := range conditions {
		if conditions[i].Type == redisv1.ConditionReplicationHealthy {
			replCond = &conditions[i]
			break
		}
	}

	assert.NotNil(t, replCond)
	assert.Equal(t, metav1.ConditionFalse, replCond.Status)
	assert.Equal(t, "ReplicasNotHealthy", replCond.Reason)
}

func TestDetermineConditions_SingleInstanceNoReplicas(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		Status: redisv1.RedisClusterStatus{
			CurrentPrimary: "test-0",
		},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
	}

	conditions := determineConditions(cluster, statuses)

	var replCond *metav1.Condition
	for i := range conditions {
		if conditions[i].Type == redisv1.ConditionReplicationHealthy {
			replCond = &conditions[i]
			break
		}
	}

	// Single instance: len(statuses) == 1, so ReplicationHealthy should be false.
	assert.NotNil(t, replCond)
	assert.Equal(t, metav1.ConditionFalse, replCond.Status)
}

func TestCheckReachability_AllReachable(t *testing.T) {
	r := &ClusterReconciler{}
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{Instances: 2},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Connected: true},
		"test-1": {Connected: true},
	}

	requeue := r.checkReachability(nil, cluster, statuses)
	assert.False(t, requeue)
}

func TestCheckReachability_SomeUnreachable(t *testing.T) {
	r := &ClusterReconciler{}
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{Instances: 3},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Connected: true},
		"test-1": {Connected: false},
	}

	requeue := r.checkReachability(nil, cluster, statuses)
	assert.True(t, requeue)
}

func TestCheckReachability_FewerStatusesThanExpected(t *testing.T) {
	r := &ClusterReconciler{}
	cluster := &redisv1.RedisCluster{
		Spec: redisv1.RedisClusterSpec{Instances: 3},
	}
	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Connected: true},
	}

	requeue := r.checkReachability(nil, cluster, statuses)
	assert.True(t, requeue)
}

func TestPollPodStatus_Success(t *testing.T) {
	resp := podStatusResponse{
		Role:              "master",
		ReplicationOffset: 12345,
		ConnectedReplicas: 2,
		MasterLinkStatus:  "",
		Connected:         true,
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		assert.Equal(t, "/v1/status", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	httpClient := &http.Client{}
	status, err := pollPodStatus(context.Background(), httpClient, srv.URL+"/v1/status")
	require.NoError(t, err)

	assert.Equal(t, "master", status.Role)
	assert.Equal(t, int64(12345), status.ReplicationOffset)
	assert.Equal(t, 2, status.ConnectedReplicas)
	assert.True(t, status.Connected)
}

func TestPollPodStatus_Non200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	httpClient := &http.Client{}
	_, err := pollPodStatus(context.Background(), httpClient, srv.URL+"/v1/status")
	require.Error(t, err)
	assert.Contains(t, err.Error(), "returned 500")
}

func TestUpdateStatus_PatchesCorrectly(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-0"

	r, c := newReconciler(cluster)
	ctx := context.Background()

	statuses := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true, ReplicationOffset: 10000},
		"test-1": {Role: "slave", Connected: true, ReplicationOffset: 9500, MasterLinkStatus: "up"},
		"test-2": {Role: "slave", Connected: false},
	}

	err := r.updateStatus(ctx, cluster, statuses)
	require.NoError(t, err)

	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)

	assert.Equal(t, int32(2), updated.Status.ReadyInstances, "2 out of 3 are connected")
	assert.Equal(t, int32(3), updated.Status.Instances)
	assert.Equal(t, redisv1.ClusterPhaseDegraded, updated.Status.Phase, "one pod disconnected means degraded")
}

func TestUpdateStatus_CountsReadyInstances(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-0"

	r, c := newReconciler(cluster)
	ctx := context.Background()

	allConnected := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
		"test-1": {Role: "slave", Connected: true, MasterLinkStatus: "up"},
		"test-2": {Role: "slave", Connected: true, MasterLinkStatus: "up"},
	}

	err := r.updateStatus(ctx, cluster, allConnected)
	require.NoError(t, err)

	var updated redisv1.RedisCluster
	err = c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &updated)
	require.NoError(t, err)

	assert.Equal(t, int32(3), updated.Status.ReadyInstances)
	assert.Equal(t, redisv1.ClusterPhaseHealthy, updated.Status.Phase)
}

func TestPollInstanceStatuses_NoPodIP(t *testing.T) {
	cluster := newTestCluster("test", "default", 2)

	// Pod with no IP should be skipped.
	pods := []client.Object{
		&corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "test-0",
				Namespace: "default",
				Labels:    map[string]string{redisv1.LabelCluster: "test"},
			},
			Status: corev1.PodStatus{PodIP: ""},
		},
	}

	r, _ := newReconciler(append([]client.Object{cluster}, pods...)...)
	ctx := context.Background()

	statuses, err := r.pollInstanceStatuses(ctx, cluster)
	require.NoError(t, err)

	// Pod with empty IP should not appear in statuses.
	assert.Empty(t, statuses)
}
