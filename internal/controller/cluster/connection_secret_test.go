package cluster

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestBuildConnectionSecretData(t *testing.T) {
	tests := []struct {
		name     string
		mutate   func(*redisv1.RedisCluster)
		password string
		// wantKeys is the exact set of keys expected in the payload.
		wantKeys []string
		want     map[string]string // subset assertions on values
	}{
		{
			name:     "standalone with password",
			password: "s3cr3t",
			wantKeys: []string{
				"cluster_name", "namespace", "mode", "host", "port", "username", "password",
				"url", "leader_host", "leader_url", "replica_host", "replica_url",
			},
			want: map[string]string{
				"cluster_name": "rc",
				"namespace":    "ns",
				"mode":         "standalone",
				"host":         "rc-leader.ns.svc",
				"port":         "6379",
				"username":     "default",
				"password":     "s3cr3t",
				"url":          "redis://default:s3cr3t@rc-leader.ns.svc:6379",
				"leader_host":  "rc-leader.ns.svc",
				"leader_url":   "redis://default:s3cr3t@rc-leader.ns.svc:6379",
				"replica_host": "rc-replica.ns.svc",
				"replica_url":  "redis://default:s3cr3t@rc-replica.ns.svc:6379",
			},
		},
		{
			name:     "standalone without password omits password but keeps url",
			password: "",
			wantKeys: []string{
				"cluster_name", "namespace", "mode", "host", "port", "username",
				"url", "leader_host", "leader_url", "replica_host", "replica_url",
			},
			want: map[string]string{
				"url":        "redis://rc-leader.ns.svc:6379",
				"leader_url": "redis://rc-leader.ns.svc:6379",
			},
		},
		{
			name:     "standalone with TLS uses rediss scheme",
			password: "pw",
			mutate: func(c *redisv1.RedisCluster) {
				c.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls"}
				c.Spec.CASecret = &redisv1.LocalObjectReference{Name: "ca"}
			},
			want: map[string]string{
				"url": "rediss://default:pw@rc-leader.ns.svc:6379",
			},
		},
		{
			name:     "sentinel adds discovery keys",
			password: "pw",
			mutate: func(c *redisv1.RedisCluster) {
				c.Spec.Mode = redisv1.ClusterModeSentinel
				c.Spec.Instances = 3
			},
			wantKeys: []string{
				"cluster_name", "namespace", "mode", "host", "port", "username", "password",
				"url", "leader_host", "leader_url", "replica_host", "replica_url",
				"sentinel_host", "sentinel_port", "master_name",
			},
			want: map[string]string{
				"mode":          "sentinel",
				"sentinel_host": "rc-sentinel.ns.svc",
				"sentinel_port": "26379",
				"master_name":   "rc",
			},
		},
		{
			name:     "sentinel with TLS uses rediss scheme and keeps discovery keys",
			password: "pw",
			mutate: func(c *redisv1.RedisCluster) {
				c.Spec.Mode = redisv1.ClusterModeSentinel
				c.Spec.Instances = 3
				c.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls"}
				c.Spec.CASecret = &redisv1.LocalObjectReference{Name: "ca"}
			},
			wantKeys: []string{
				"cluster_name", "namespace", "mode", "host", "port", "username", "password",
				"url", "leader_host", "leader_url", "replica_host", "replica_url",
				"sentinel_host", "sentinel_port", "master_name",
			},
			want: map[string]string{
				"mode":          "sentinel",
				"url":           "rediss://default:pw@rc-leader.ns.svc:6379",
				"leader_url":    "rediss://default:pw@rc-leader.ns.svc:6379",
				"replica_url":   "rediss://default:pw@rc-replica.ns.svc:6379",
				"sentinel_host": "rc-sentinel.ns.svc",
				"sentinel_port": "26379",
				"master_name":   "rc",
			},
		},
		{
			name:     "cluster mode uses headless service and omits leader/replica",
			password: "pw",
			mutate: func(c *redisv1.RedisCluster) {
				c.Spec.Mode = redisv1.ClusterModeCluster
				c.Spec.Instances = 0
				c.Spec.Shards = 3
			},
			wantKeys: []string{
				"cluster_name", "namespace", "mode", "host", "port", "username", "password", "url",
			},
			want: map[string]string{
				"mode": "cluster",
				"host": "rc-cluster.ns.svc",
				"url":  "redis://default:pw@rc-cluster.ns.svc:6379",
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := newTestCluster("rc", "ns", 1)
			if tt.mutate != nil {
				tt.mutate(cluster)
			}

			data := buildConnectionSecretData(cluster, tt.password)

			if tt.wantKeys != nil {
				gotKeys := make([]string, 0, len(data))
				for k := range data {
					gotKeys = append(gotKeys, k)
				}
				assert.ElementsMatch(t, tt.wantKeys, gotKeys)
			}
			for k, v := range tt.want {
				assert.Equalf(t, v, string(data[k]), "key %q", k)
			}

			// Cluster mode must never expose leader/replica endpoints.
			if cluster.Spec.Mode == redisv1.ClusterModeCluster {
				assert.NotContains(t, data, "leader_host")
				assert.NotContains(t, data, "replica_host")
			}
		})
	}
}

func TestBuildConnectionSecretData_EscapesPassword(t *testing.T) {
	cluster := newTestCluster("rc", "ns", 1)
	data := buildConnectionSecretData(cluster, "p@ss:w/rd")
	// The userinfo must be percent-encoded so the URL stays parseable.
	assert.Equal(t, "redis://default:p%40ss%3Aw%2Frd@rc-leader.ns.svc:6379", string(data["url"]))
	// The raw password key is unescaped for direct consumption.
	assert.Equal(t, "p@ss:w/rd", string(data["password"]))
}

func TestReconcileConnectionSecret_NilSpecIsNoOp(t *testing.T) {
	cluster := newTestCluster("rc", "ns", 1)
	r, c := newReconciler(cluster)
	require.NoError(t, r.reconcileConnectionSecret(context.Background(), cluster))

	var list corev1.SecretList
	require.NoError(t, c.List(context.Background(), &list))
	assert.Empty(t, list.Items)
}

func TestReconcileConnectionSecret_CreatesAndOwns(t *testing.T) {
	cluster := newTestCluster("rc", "ns", 1)
	cluster.UID = "cluster-uid"
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "rc-auth"}
	cluster.Spec.ConnectionSecret = &redisv1.ConnectionSecretSpec{Name: "rc-conn"}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rc-auth", Namespace: "ns"},
		Data:       map[string][]byte{"password": []byte("topsecret")},
	}

	r, c := newReconciler(cluster, authSecret)
	require.NoError(t, r.reconcileConnectionSecret(context.Background(), cluster))

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "rc-conn", Namespace: "ns"}, &got))
	assert.Equal(t, corev1.SecretTypeOpaque, got.Type)
	assert.Equal(t, "topsecret", string(got.Data["password"]))
	assert.Equal(t, "redis://default:topsecret@rc-leader.ns.svc:6379", string(got.Data["url"]))
	assert.Equal(t, "rc", got.Labels[redisv1.LabelCluster])
	assert.True(t, metav1.IsControlledBy(&got, cluster), "connection secret must be owned by the cluster")
}

func TestReconcileConnectionSecret_PropagatesRotation(t *testing.T) {
	cluster := newTestCluster("rc", "ns", 1)
	cluster.UID = "cluster-uid"
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "rc-auth"}
	cluster.Spec.ConnectionSecret = &redisv1.ConnectionSecretSpec{Name: "rc-conn"}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rc-auth", Namespace: "ns"},
		Data:       map[string][]byte{"password": []byte("old")},
	}

	r, c := newReconciler(cluster, authSecret)
	ctx := context.Background()
	require.NoError(t, r.reconcileConnectionSecret(ctx, cluster))

	// Rotate the auth password.
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "rc-auth", Namespace: "ns"}, authSecret))
	authSecret.Data["password"] = []byte("new")
	require.NoError(t, c.Update(ctx, authSecret))

	require.NoError(t, r.reconcileConnectionSecret(ctx, cluster))

	var got corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "rc-conn", Namespace: "ns"}, &got))
	assert.Equal(t, "new", string(got.Data["password"]))
	assert.Equal(t, "redis://default:new@rc-leader.ns.svc:6379", string(got.Data["url"]))
}

func TestReconcileConnectionSecret_DeletesWhenDisabled(t *testing.T) {
	cluster := newTestCluster("rc", "ns", 1)
	cluster.UID = "cluster-uid"
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "rc-auth"}
	cluster.Spec.ConnectionSecret = &redisv1.ConnectionSecretSpec{Name: "rc-conn"}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rc-auth", Namespace: "ns"},
		Data:       map[string][]byte{"password": []byte("pw")},
	}

	r, c := newReconciler(cluster, authSecret)
	ctx := context.Background()
	require.NoError(t, r.reconcileConnectionSecret(ctx, cluster))

	// Disable the feature; the credential-bearing Secret must be removed.
	cluster.Spec.ConnectionSecret = nil
	require.NoError(t, r.reconcileConnectionSecret(ctx, cluster))

	var got corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Name: "rc-conn", Namespace: "ns"}, &got)
	assert.True(t, apierrors.IsNotFound(err), "connection secret should be deleted, got err=%v", err)
}

func TestReconcileConnectionSecret_DeletesStaleAfterRename(t *testing.T) {
	cluster := newTestCluster("rc", "ns", 1)
	cluster.UID = "cluster-uid"
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "rc-auth"}
	cluster.Spec.ConnectionSecret = &redisv1.ConnectionSecretSpec{Name: "rc-conn-old"}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rc-auth", Namespace: "ns"},
		Data:       map[string][]byte{"password": []byte("pw")},
	}

	r, c := newReconciler(cluster, authSecret)
	ctx := context.Background()
	require.NoError(t, r.reconcileConnectionSecret(ctx, cluster))

	// Rename the target Secret.
	cluster.Spec.ConnectionSecret.Name = "rc-conn-new"
	require.NoError(t, r.reconcileConnectionSecret(ctx, cluster))

	var newSecret corev1.Secret
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "rc-conn-new", Namespace: "ns"}, &newSecret))

	var oldSecret corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Name: "rc-conn-old", Namespace: "ns"}, &oldSecret)
	assert.True(t, apierrors.IsNotFound(err), "stale connection secret should be deleted, got err=%v", err)
}

func TestReconcileConnectionSecret_PrunesOwnedSecretWithoutMarker(t *testing.T) {
	cluster := newTestCluster("rc", "ns", 1)
	cluster.UID = "cluster-uid"
	// Feature is disabled (no spec.connectionSecret).

	// A connection Secret published by a pre-marker version: owned via owner
	// reference and cluster-labeled, but missing the connection-secret marker.
	legacy := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "rc-conn",
			Namespace: "ns",
			Labels:    map[string]string{redisv1.LabelCluster: "rc"},
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: redisv1.GroupVersion.String(),
				Kind:       "RedisCluster",
				Name:       cluster.Name,
				UID:        cluster.UID,
				Controller: boolPtr(true),
			}},
		},
		Data: map[string][]byte{"url": []byte("redis://old"), "password": []byte("pw")},
	}

	r, c := newReconciler(cluster, legacy)
	ctx := context.Background()
	require.NoError(t, r.reconcileConnectionSecret(ctx, cluster))

	var got corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Name: "rc-conn", Namespace: "ns"}, &got)
	assert.True(t, apierrors.IsNotFound(err), "owned pre-marker connection secret should be pruned, got err=%v", err)
}

func TestReconcileConnectionSecret_RefusesToOverwriteUnowned(t *testing.T) {
	cluster := newTestCluster("rc", "ns", 1)
	cluster.UID = "cluster-uid"
	cluster.Spec.AuthSecret = &redisv1.LocalObjectReference{Name: "rc-auth"}
	cluster.Spec.ConnectionSecret = &redisv1.ConnectionSecretSpec{Name: "rc-conn"}

	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rc-auth", Namespace: "ns"},
		Data:       map[string][]byte{"password": []byte("pw")},
	}
	// A pre-existing, user-owned Secret sharing the configured name.
	foreign := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "rc-conn", Namespace: "ns"},
		Data:       map[string][]byte{"keep": []byte("me")},
	}

	r, c, recorder := newReconcilerWithRecorder(cluster, authSecret, foreign)
	require.NoError(t, r.reconcileConnectionSecret(context.Background(), cluster))

	var got corev1.Secret
	require.NoError(t, c.Get(context.Background(), types.NamespacedName{Name: "rc-conn", Namespace: "ns"}, &got))
	// Untouched: original data preserved, no connection keys written.
	assert.Equal(t, "me", string(got.Data["keep"]))
	assert.NotContains(t, got.Data, "url")

	select {
	case event := <-recorder.Events:
		assert.Contains(t, event, "ConnectionSecretConflict")
	default:
		t.Fatal("expected a ConnectionSecretConflict event")
	}
}
