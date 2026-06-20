package cluster

// These tests pin the integration contract documented in docs/service-contract.md.
// External platforms (Hostess and similar) depend on the generated Service names,
// selectors, ports, and the compatibility-committed pod labels. Changing any
// assertion here is a breaking change to that contract: update the doc in the
// same change and treat it as such.

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// assertService fetches a Service and asserts its selector and single redis port.
func assertService(t *testing.T, c client.Client, namespace, name string, selector map[string]string, port int32) {
	t.Helper()
	var svc corev1.Service
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, &svc),
		"service %s should exist", name)
	assert.Equal(t, selector, svc.Spec.Selector, "service %s selector", name)
	require.Len(t, svc.Spec.Ports, 1, "service %s ports", name)
	assert.Equal(t, port, svc.Spec.Ports[0].Port, "service %s port", name)
}

// assertNoService asserts a Service does not exist for the given mode.
func assertNoService(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	var svc corev1.Service
	err := c.Get(context.Background(), types.NamespacedName{Name: name, Namespace: namespace}, &svc)
	assert.True(t, errors.IsNotFound(err), "service %s should not exist, got err=%v", name, err)
}

// Compatibility-committed label keys and values. These string literals are the
// contract; the api/v1 constants merely reference them. If a constant is renamed
// or its value changes, this test fails on purpose.
func TestLabelContract_CommittedKeysAndValues(t *testing.T) {
	assert.Equal(t, "redis.io/cluster", redisv1.LabelCluster)
	assert.Equal(t, "redis.io/instance", redisv1.LabelInstance)
	assert.Equal(t, "redis.io/role", redisv1.LabelRole)
	assert.Equal(t, "redis.io/workload", redisv1.LabelWorkload)

	assert.Equal(t, "primary", redisv1.LabelRolePrimary)
	assert.Equal(t, "replica", redisv1.LabelRoleReplica)
	assert.Equal(t, "sentinel", redisv1.LabelRoleSentinel)
	assert.Equal(t, "data", redisv1.LabelWorkloadData)
	assert.Equal(t, "sentinel", redisv1.LabelWorkloadSentinel)
}

// podLabels is the source of the labels every data/sentinel pod carries, and the
// thing Service selectors match against. Assert the committed keys are always present.
func TestLabelContract_PodLabels(t *testing.T) {
	data := podLabels("mycluster", "mycluster-0", redisv1.LabelRolePrimary)
	assert.Equal(t, "mycluster", data[redisv1.LabelCluster])
	assert.Equal(t, "mycluster-0", data[redisv1.LabelInstance])
	assert.Equal(t, redisv1.LabelRolePrimary, data[redisv1.LabelRole])
	assert.Equal(t, redisv1.LabelWorkloadData, data[redisv1.LabelWorkload])

	sentinel := podLabels("mycluster", "mycluster-sentinel-0", redisv1.LabelRoleSentinel)
	assert.Equal(t, redisv1.LabelRoleSentinel, sentinel[redisv1.LabelRole])
	assert.Equal(t, redisv1.LabelWorkloadSentinel, sentinel[redisv1.LabelWorkload])
}

func TestServiceContract_StandaloneMode(t *testing.T) {
	cluster := newTestCluster("mycluster", "default", 3)
	r, c := newReconciler(cluster)
	ctx := context.Background()

	require.NoError(t, r.reconcileServices(ctx, cluster))

	assertService(t, c, "default", "mycluster-leader", map[string]string{
		redisv1.LabelCluster: "mycluster",
		redisv1.LabelRole:    redisv1.LabelRolePrimary,
	}, 6379)
	assertService(t, c, "default", "mycluster-replica", map[string]string{
		redisv1.LabelCluster: "mycluster",
		redisv1.LabelRole:    redisv1.LabelRoleReplica,
	}, 6379)
	assertService(t, c, "default", "mycluster-any", map[string]string{
		redisv1.LabelCluster:  "mycluster",
		redisv1.LabelWorkload: redisv1.LabelWorkloadData,
	}, 6379)

	assertNoService(t, c, "default", "mycluster-sentinel")
	assertNoService(t, c, "default", "mycluster-cluster")
}

// Once a primary is known, -leader narrows from role=primary to the specific pod.
func TestServiceContract_LeaderPinsToCurrentPrimary(t *testing.T) {
	cluster := newTestCluster("mycluster", "default", 3)
	cluster.Status.CurrentPrimary = "mycluster-0"
	r, c := newReconciler(cluster)
	ctx := context.Background()

	require.NoError(t, r.reconcileServices(ctx, cluster))

	assertService(t, c, "default", "mycluster-leader", map[string]string{
		redisv1.LabelCluster:  "mycluster",
		redisv1.LabelInstance: "mycluster-0",
	}, 6379)
}

func TestServiceContract_SentinelMode(t *testing.T) {
	cluster := newTestCluster("mycluster", "default", 3)
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	r, c := newReconciler(cluster)
	ctx := context.Background()

	require.NoError(t, r.reconcileServices(ctx, cluster))

	assertService(t, c, "default", "mycluster-leader", map[string]string{
		redisv1.LabelCluster: "mycluster",
		redisv1.LabelRole:    redisv1.LabelRolePrimary,
	}, 6379)
	assertService(t, c, "default", "mycluster-replica", map[string]string{
		redisv1.LabelCluster: "mycluster",
		redisv1.LabelRole:    redisv1.LabelRoleReplica,
	}, 6379)
	assertService(t, c, "default", "mycluster-any", map[string]string{
		redisv1.LabelCluster:  "mycluster",
		redisv1.LabelWorkload: redisv1.LabelWorkloadData,
	}, 6379)
	assertService(t, c, "default", "mycluster-sentinel", map[string]string{
		redisv1.LabelCluster: "mycluster",
		redisv1.LabelRole:    redisv1.LabelRoleSentinel,
	}, redisv1.SentinelPort)

	assertNoService(t, c, "default", "mycluster-cluster")
}

func TestServiceContract_ClusterMode(t *testing.T) {
	cluster := newTestCluster("mycluster", "default", 0)
	cluster.Spec.Mode = redisv1.ClusterModeCluster
	cluster.Spec.Shards = 3
	r, c := newReconciler(cluster)
	ctx := context.Background()

	require.NoError(t, r.reconcileServices(ctx, cluster))

	// -any selects every data pod across all shards.
	assertService(t, c, "default", "mycluster-any", map[string]string{
		redisv1.LabelCluster:  "mycluster",
		redisv1.LabelWorkload: redisv1.LabelWorkloadData,
	}, 6379)

	// -cluster is the headless discovery + bus service.
	var headless corev1.Service
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "mycluster-cluster", Namespace: "default"}, &headless))
	assert.Equal(t, corev1.ClusterIPNone, headless.Spec.ClusterIP)
	assert.Equal(t, map[string]string{
		redisv1.LabelCluster:  "mycluster",
		redisv1.LabelWorkload: redisv1.LabelWorkloadData,
	}, headless.Spec.Selector)
	require.Len(t, headless.Spec.Ports, 2)
	assert.Equal(t, int32(6379), headless.Spec.Ports[0].Port)
	assert.Equal(t, int32(16379), headless.Spec.Ports[1].Port)

	// Sentinel/leader/replica services are not part of cluster mode.
	assertNoService(t, c, "default", "mycluster-leader")
	assertNoService(t, c, "default", "mycluster-replica")
	assertNoService(t, c, "default", "mycluster-sentinel")
}
