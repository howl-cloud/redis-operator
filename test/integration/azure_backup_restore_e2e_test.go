//go:build integration

package integration

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	restorecmd "github.com/howl-cloud/redis-operator/internal/cmd/manager/restore"
)

func TestAzureBackupRestoreThroughOperatorReconciler(t *testing.T) {
	requireIntegrationDocker(t)

	tests := []struct {
		name         string
		method       redisv1.BackupMethod
		mode         persistenceMode
		keyPrefix    string
		objectPrefix string
		artifactType redisv1.BackupArtifactType
	}{
		{
			name:         "rdb",
			method:       redisv1.BackupMethodRDB,
			mode:         persistenceRDB,
			keyPrefix:    "azure-rdb-cycle",
			objectPrefix: "e2e/azure/rdb",
			artifactType: redisv1.BackupArtifactTypeRDB,
		},
		{
			name:         "aof",
			method:       redisv1.BackupMethodAOF,
			mode:         persistenceAOF,
			keyPrefix:    "azure-aof-cycle",
			objectPrefix: "e2e/azure/aof",
			artifactType: redisv1.BackupArtifactTypeAOFArchive,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			env := newOperatorBackupEnvironment(ctx, t)
			configureRestoreKubeconfig(t, env.cfg)

			azuriteInfo := startAzuriteContainer(ctx, t)
			backupSecretName := newUniqueName("azure-backup-creds")
			createAzureBackupCredentialsSecret(ctx, t, env.k8sClient, operatorBackupNamespace, backupSecretName, azuriteInfo)

			sourceDataDir := mustTempDir(t, "redis-azure-source-"+tc.name+"-")
			restoredDataDir := mustTempDir(t, "redis-azure-restored-"+tc.name+"-")
			source := startRedisWithDataDir(ctx, t, sourceDataDir, tc.mode)
			sourceClient := newRedisClient(ctx, t, source, "", "")
			waitForRedisReady(t, ctx, sourceClient)

			writeDataset(t, ctx, sourceClient, tc.keyPrefix, backupRestoreKeyCount)
			if tc.method == redisv1.BackupMethodAOF {
				triggerAndWaitBGREWRITEAOF(t, ctx, sourceClient)
			}

			clusterName := newUniqueName("azure-source")
			primaryPodName := clusterName + "-0"
			createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, 1, backupSecretName)
			markRedisClusterHealthy(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, primaryPodName)
			createClusterPod(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, primaryPodName, "127.0.0.1")

			startBackupAPIServer(ctx, t, backupEndpointSource{
				container:   source,
				redisClient: sourceClient,
				azuriteInfo: &azuriteInfo,
			})

			backupName := newUniqueName("azure-backup")
			createAzureRedisBackup(
				ctx,
				t,
				env.k8sClient,
				operatorBackupNamespace,
				backupName,
				clusterName,
				redisv1.BackupTargetPrimary,
				tc.method,
				azuriteInfo,
				tc.objectPrefix,
			)

			completed := reconcileBackupUntilCompleted(ctx, t, env, operatorBackupNamespace, backupName)
			require.Equal(t, tc.artifactType, completed.Status.ArtifactType)
			require.True(t, strings.HasPrefix(completed.Status.BackupPath, "azblob://"), "expected azblob:// path, got %q", completed.Status.BackupPath)

			require.NoError(t, source.Terminate(ctx))

			restoreClusterName := newUniqueName("azure-restored")
			createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, restoreClusterName, 1, backupSecretName)
			require.NoError(t, restorecmd.Run(ctx, restoreClusterName, backupName, operatorBackupNamespace, restoredDataDir, ""))

			restored := startRedisWithDataDir(ctx, t, restoredDataDir, tc.mode)
			restoredClient := newRedisClient(ctx, t, restored, "", "")
			waitForRedisReady(t, ctx, restoredClient)

			assertDatasetEventually(t, ctx, restoredClient, tc.keyPrefix, backupRestoreKeyCount)
			assertDBSizeEventually(t, ctx, restoredClient, backupRestoreKeyCount)
		})
	}
}

func createAzureBackupCredentialsSecret(
	ctx context.Context,
	t *testing.T,
	k8sClient ctrlclient.Client,
	namespace, name string,
	info AzuriteInfo,
) {
	t.Helper()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"AZURE_STORAGE_CONNECTION_STRING": []byte(info.ConnectionString),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
}

func createAzureRedisBackup(
	ctx context.Context,
	t *testing.T,
	k8sClient ctrlclient.Client,
	namespace, backupName, clusterName string,
	target redisv1.BackupTarget,
	method redisv1.BackupMethod,
	info AzuriteInfo,
	pathPrefix string,
) {
	t.Helper()

	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupName,
			Namespace: namespace,
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: clusterName,
			Target:      target,
			Method:      method,
			Destination: &redisv1.BackupDestination{
				Azure: &redisv1.AzureBlobDestination{
					Container:   info.Container,
					Path:        pathPrefix,
					AccountName: info.AccountName,
					Endpoint:    info.Endpoint,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, backup))
}
