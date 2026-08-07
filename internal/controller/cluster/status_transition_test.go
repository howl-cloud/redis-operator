package cluster

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// A steady-state reconcile must not rewrite LastTransitionTime, otherwise every
// status patch differs from the stored object and writes for no reason.
func TestUpdateStatus_StableConditionsKeepTransitionTime(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-0"

	r, c := newReconciler(cluster)
	ctx := context.Background()

	allConnected := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
		"test-1": {Role: "slave", Connected: true, MasterLinkStatus: "up"},
		"test-2": {Role: "slave", Connected: true, MasterLinkStatus: "up"},
	}

	require.NoError(t, r.updateStatus(ctx, cluster, allConnected))

	var first redisv1.RedisCluster
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &first))
	require.NotEmpty(t, first.Status.Conditions, "expected conditions to be set")

	firstStamps := map[string]string{}
	for _, cond := range first.Status.Conditions {
		firstStamps[cond.Type] = cond.LastTransitionTime.String()
	}

	// metav1.Time has second granularity, so without the fix a same-second
	// re-reconcile could coincidentally match. Cross a second boundary.
	time.Sleep(1100 * time.Millisecond)

	require.NoError(t, r.updateStatus(ctx, &first, allConnected))

	var second redisv1.RedisCluster
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &second))

	for _, cond := range second.Status.Conditions {
		assert.Equal(t, firstStamps[cond.Type], cond.LastTransitionTime.String(),
			"condition %q kept its status, so LastTransitionTime must not move", cond.Type)
	}
	assert.Equal(t, first.ResourceVersion, second.ResourceVersion,
		"an unchanged status must produce an empty patch, leaving resourceVersion alone")
}

// The timestamp must still move when the condition actually transitions,
// otherwise the fix would defeat the field's purpose.
func TestUpdateStatus_ChangedConditionMovesTransitionTime(t *testing.T) {
	cluster := newTestCluster("test", "default", 3)
	cluster.Status.CurrentPrimary = "test-0"

	r, c := newReconciler(cluster)
	ctx := context.Background()

	allConnected := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: true},
		"test-1": {Role: "slave", Connected: true, MasterLinkStatus: "up"},
		"test-2": {Role: "slave", Connected: true, MasterLinkStatus: "up"},
	}
	require.NoError(t, r.updateStatus(ctx, cluster, allConnected))

	var healthy redisv1.RedisCluster
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &healthy))

	readyBefore := findCondition(t, healthy.Status.Conditions, redisv1.ConditionReady)
	require.Equal(t, "True", string(readyBefore.Status), "expected a healthy cluster to start Ready")

	time.Sleep(1100 * time.Millisecond)

	// Ready is True whenever any master is connected, so the primary itself has
	// to drop for the condition to flip.
	degraded := map[string]redisv1.InstanceStatus{
		"test-0": {Role: "master", Connected: false},
		"test-1": {Role: "slave", Connected: false},
		"test-2": {Role: "slave", Connected: false},
	}
	require.NoError(t, r.updateStatus(ctx, &healthy, degraded))

	var after redisv1.RedisCluster
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "test", Namespace: "default"}, &after))

	readyAfter := findCondition(t, after.Status.Conditions, redisv1.ConditionReady)
	assert.NotEqual(t, string(readyBefore.Status), string(readyAfter.Status),
		"expected the Ready condition to flip when instances disconnect")
	assert.True(t, readyAfter.LastTransitionTime.After(readyBefore.LastTransitionTime.Time),
		"a real transition must advance LastTransitionTime")
}

func findCondition(t *testing.T, conditions []metav1.Condition, condType string) metav1.Condition {
	t.Helper()
	for i := range conditions {
		if conditions[i].Type == condType {
			return conditions[i]
		}
	}
	t.Fatalf("condition %q not found", condType)
	return metav1.Condition{}
}
