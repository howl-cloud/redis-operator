//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	clustercontroller "github.com/howl-cloud/redis-operator/internal/controller/cluster"
	instreconciler "github.com/howl-cloud/redis-operator/internal/instance-manager/reconciler"
)

func TestCrossClusterReplicationReplicaModeFlow(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	network, err := tcnetwork.New(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = network.Remove(context.Background())
	})

	primary := startRedisContainer(
		ctx,
		t,
		tcnetwork.WithNetwork([]string{"primary"}, network),
	)
	replica := startRedisContainer(
		ctx,
		t,
		tcnetwork.WithNetwork([]string{"replica"}, network),
	)

	primaryClient := newRedisClient(ctx, t, primary, "", "")
	replicaClient := newRedisClient(ctx, t, replica, "", "")
	waitForRedisReady(t, ctx, primaryClient)
	waitForRedisReady(t, ctx, replicaClient)

	keyPrefix := "cross-cluster-dr"
	writeDataset(t, ctx, primaryClient, keyPrefix, 100)

	const (
		clusterName    = "dr-replica-cluster"
		replicaPodName = "dr-replica-cluster-0"
		namespace      = "default"
		statusServerIP = "127.0.0.1"
		statusServer   = "127.0.0.1:8080"
	)

	replicaCluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      clusterName,
			Namespace: namespace,
		},
		Spec: redisv1.RedisClusterSpec{
			Instances: 1,
			Storage: redisv1.StorageSpec{
				Size: resource.MustParse("1Gi"),
			},
			ReplicaMode: &redisv1.ReplicaModeSpec{
				Enabled: true,
				Source: &redisv1.ReplicaSourceSpec{
					ClusterName: "primary-cluster",
					Host:        "primary",
					Port:        6379,
				},
			},
		},
		Status: redisv1.RedisClusterStatus{
			CurrentPrimary: replicaPodName,
		},
	}
	replicaPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      replicaPodName,
			Namespace: namespace,
			Labels: map[string]string{
				redisv1.LabelCluster:  clusterName,
				redisv1.LabelInstance: replicaPodName,
				redisv1.LabelRole:     redisv1.LabelRolePrimary,
				redisv1.LabelWorkload: redisv1.LabelWorkloadData,
			},
		},
		Status: corev1.PodStatus{
			PodIP: statusServerIP,
		},
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(redisv1.AddToScheme(scheme))
	k8sClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(replicaCluster, replicaPod).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		Build()

	clusterRecorder := record.NewFakeRecorder(100)
	clusterReconciler := clustercontroller.NewClusterReconciler(k8sClient, scheme, clusterRecorder, 0)
	instanceReconciler := instreconciler.NewInstanceReconciler(
		k8sClient,
		replicaClient,
		record.NewFakeRecorder(100),
		clusterName,
		replicaPodName,
		namespace,
	)

	_, err = instanceReconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: clusterName, Namespace: namespace},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, infoErr := replicaClient.Info(ctx, "replication").Result()
		return infoErr == nil &&
			containsRedisInfoField(info, "role:slave") &&
			containsRedisInfoField(info, "master_link_status:up")
	}, 30*time.Second, 250*time.Millisecond)
	assertDatasetEventually(t, ctx, replicaClient, keyPrefix, 100)

	err = replicaClient.Set(ctx, "cross-cluster:readonly", "should-fail", 0).Err()
	require.Error(t, err)
	require.Contains(t, strings.ToUpper(err.Error()), "READONLY")

	var current redisv1.RedisCluster
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, &current))
	current.Spec.ReplicaMode.Promote = true
	require.NoError(t, k8sClient.Update(ctx, &current))

	_, err = instanceReconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: clusterName, Namespace: namespace},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		info, infoErr := replicaClient.Info(ctx, "replication").Result()
		return infoErr == nil && containsRedisInfoField(info, "role:master")
	}, 30*time.Second, 250*time.Millisecond)

	startMockInstanceStatusServer(t, statusServer, integrationStatusResponse{
		Role:              "master",
		ReplicationOffset: 1000,
		ConnectedReplicas: 0,
		MasterLinkStatus:  "",
		Connected:         true,
	})

	request := reconcile.Request{
		NamespacedName: types.NamespacedName{Name: clusterName, Namespace: namespace},
	}
	require.Eventually(t, func() bool {
		if _, reconcileErr := clusterReconciler.Reconcile(ctx, request); reconcileErr != nil {
			return false
		}
		var updated redisv1.RedisCluster
		if getErr := k8sClient.Get(ctx, request.NamespacedName, &updated); getErr != nil {
			return false
		}
		return updated.Spec.ReplicaMode != nil &&
			!updated.Spec.ReplicaMode.Enabled &&
			!updated.Spec.ReplicaMode.Promote &&
			hasReplicaModePromotedCondition(updated.Status.Conditions)
	}, 15*time.Second, 250*time.Millisecond)
	require.Eventually(t, func() bool {
		return recorderContainsEvent(clusterRecorder, "ReplicaClusterPromoted")
	}, 5*time.Second, 100*time.Millisecond)

	require.NoError(t, replicaClient.Set(ctx, "cross-cluster:post-promote", "ok", 0).Err())
	value, err := replicaClient.Get(ctx, "cross-cluster:post-promote").Result()
	require.NoError(t, err)
	require.Equal(t, "ok", value)
}

type integrationStatusResponse struct {
	Role              string `json:"role"`
	ReplicationOffset int64  `json:"replicationOffset"`
	ConnectedReplicas int    `json:"connectedReplicas"`
	MasterLinkStatus  string `json:"masterLinkStatus,omitempty"`
	Connected         bool   `json:"connected"`
}

func startMockInstanceStatusServer(t *testing.T, addr string, response integrationStatusResponse) {
	t.Helper()

	listener, err := net.Listen("tcp", addr)
	require.NoError(t, err)

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})
	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		_ = server.Serve(listener)
	}()

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	})
}

func hasReplicaModePromotedCondition(conditions []metav1.Condition) bool {
	for i := range conditions {
		if conditions[i].Type == redisv1.ConditionReplicaMode &&
			conditions[i].Status == metav1.ConditionFalse &&
			conditions[i].Reason == "ReplicaClusterPromoted" {
			return true
		}
	}
	return false
}

func recorderContainsEvent(recorder *record.FakeRecorder, reason string) bool {
	found := false
	for {
		select {
		case event := <-recorder.Events:
			if strings.Contains(event, reason) {
				found = true
			}
		default:
			return found
		}
	}
}
