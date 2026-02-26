//go:build integration

package integration

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	tcnetwork "github.com/testcontainers/testcontainers-go/network"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	clientcmdapi "k8s.io/client-go/tools/clientcmd/api"
	"k8s.io/client-go/tools/record"
	ctrlclient "sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	restorecmd "github.com/howl-cloud/redis-operator/internal/cmd/manager/restore"
	backupcontroller "github.com/howl-cloud/redis-operator/internal/controller/backup"
)

const (
	operatorBackupNamespace = "default"
	backupRestoreKeyCount   = 1000

	backupPollInterval = 250 * time.Millisecond
	backupPollTimeout  = 90 * time.Second

	backupAPIServerAddr = "127.0.0.1:8080"
)

type operatorBackupEnvironment struct {
	cfg                       *rest.Config
	k8sClient                 ctrlclient.Client
	backupReconciler          *backupcontroller.BackupReconciler
	scheduledBackupReconciler *backupcontroller.ScheduledBackupReconciler
}

type backupEndpointSource struct {
	container   testcontainers.Container
	redisClient *redis.Client
	garageInfo  GarageInfo
}

type backupAPIRequest struct {
	BackupName  string                     `json:"backupName"`
	Method      redisv1.BackupMethod       `json:"method,omitempty"`
	Destination *redisv1.BackupDestination `json:"destination,omitempty"`
}

type backupAPIResponse struct {
	ArtifactType redisv1.BackupArtifactType `json:"artifactType"`
	BackupPath   string                     `json:"backupPath"`
	BackupSize   int64                      `json:"backupSize"`
}

func TestBackupRestoreThroughOperatorReconciler(t *testing.T) {
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
			keyPrefix:    "operator-rdb-cycle",
			objectPrefix: "e2e/rdb",
			artifactType: redisv1.BackupArtifactTypeRDB,
		},
		{
			name:         "aof",
			method:       redisv1.BackupMethodAOF,
			mode:         persistenceAOF,
			keyPrefix:    "operator-aof-cycle",
			objectPrefix: "e2e/aof",
			artifactType: redisv1.BackupArtifactTypeAOFArchive,
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()

			env := newOperatorBackupEnvironment(ctx, t)
			configureRestoreKubeconfig(t, env.cfg)

			garageInfo := startGarageContainer(ctx, t)
			backupSecretName := newUniqueName("backup-creds")
			createBackupCredentialsSecret(ctx, t, env.k8sClient, operatorBackupNamespace, backupSecretName, garageInfo)

			sourceDataDir := mustTempDir(t, "redis-operator-source-"+tc.name+"-")
			restoredDataDir := mustTempDir(t, "redis-operator-restored-"+tc.name+"-")
			source := startRedisWithDataDir(ctx, t, sourceDataDir, tc.mode)
			sourceClient := newRedisClient(ctx, t, source, "", "")
			waitForRedisReady(t, ctx, sourceClient)

			writeDataset(t, ctx, sourceClient, tc.keyPrefix, backupRestoreKeyCount)
			if tc.method == redisv1.BackupMethodAOF {
				triggerAndWaitBGREWRITEAOF(t, ctx, sourceClient)
			}

			clusterName := newUniqueName("source")
			primaryPodName := clusterName + "-0"
			createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, 1, backupSecretName)
			markRedisClusterHealthy(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, primaryPodName)
			createClusterPod(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, primaryPodName, "127.0.0.1")

			startBackupAPIServer(ctx, t, backupEndpointSource{
				container:   source,
				redisClient: sourceClient,
				garageInfo:  garageInfo,
			})

			backupName := newUniqueName("backup")
			createRedisBackup(
				ctx,
				t,
				env.k8sClient,
				operatorBackupNamespace,
				backupName,
				clusterName,
				redisv1.BackupTargetPrimary,
				tc.method,
				garageInfo,
				tc.objectPrefix,
			)

			completed := reconcileBackupUntilCompleted(ctx, t, env, operatorBackupNamespace, backupName)
			require.Equal(t, tc.artifactType, completed.Status.ArtifactType)
			require.NotEmpty(t, completed.Status.BackupPath)

			require.NoError(t, source.Terminate(ctx))

			restoreClusterName := newUniqueName("restored")
			createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, restoreClusterName, 1, backupSecretName)
			require.NoError(t, restorecmd.Run(ctx, restoreClusterName, backupName, operatorBackupNamespace, restoredDataDir))

			restored := startRedisWithDataDir(ctx, t, restoredDataDir, tc.mode)
			restoredClient := newRedisClient(ctx, t, restored, "", "")
			waitForRedisReady(t, ctx, restoredClient)

			assertDatasetEventually(t, ctx, restoredClient, tc.keyPrefix, backupRestoreKeyCount)
			assertDBSizeEventually(t, ctx, restoredClient, backupRestoreKeyCount)
		})
	}
}

func TestBackupFromReplicaThroughOperatorReconciler(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	env := newOperatorBackupEnvironment(ctx, t)
	configureRestoreKubeconfig(t, env.cfg)

	garageInfo := startGarageContainer(ctx, t)
	backupSecretName := newUniqueName("backup-creds")
	createBackupCredentialsSecret(ctx, t, env.k8sClient, operatorBackupNamespace, backupSecretName, garageInfo)

	network, err := tcnetwork.New(ctx)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = network.Remove(context.Background())
	})

	primary := startRedisContainer(ctx, t, tcnetwork.WithNetwork([]string{"primary"}, network))
	replica := startRedisContainer(ctx, t, tcnetwork.WithNetwork([]string{"replica"}, network))

	primaryClient := newRedisClient(ctx, t, primary, "", "")
	replicaClient := newRedisClient(ctx, t, replica, "", "")
	waitForRedisReady(t, ctx, primaryClient)
	waitForRedisReady(t, ctx, replicaClient)

	require.NoError(t, replicaClient.SlaveOf(ctx, "primary", "6379").Err())
	require.Eventually(t, func() bool {
		info, infoErr := replicaClient.Info(ctx, "replication").Result()
		return infoErr == nil &&
			containsRedisInfoField(info, "role:slave") &&
			containsRedisInfoField(info, "master_link_status:up")
	}, 30*time.Second, 250*time.Millisecond)

	writeDataset(t, ctx, primaryClient, "operator-replica-cycle", backupRestoreKeyCount)
	assertDatasetEventually(t, ctx, replicaClient, "operator-replica-cycle", backupRestoreKeyCount)

	clusterName := newUniqueName("replica-source")
	primaryPodName := clusterName + "-0"
	replicaPodName := clusterName + "-1"
	createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, 2, backupSecretName)
	markRedisClusterHealthy(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, primaryPodName)
	createClusterPod(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, primaryPodName, "")
	createClusterPod(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, replicaPodName, "127.0.0.1")

	startBackupAPIServer(ctx, t, backupEndpointSource{
		container:   replica,
		redisClient: replicaClient,
		garageInfo:  garageInfo,
	})

	backupName := newUniqueName("backup-replica")
	createRedisBackup(
		ctx,
		t,
		env.k8sClient,
		operatorBackupNamespace,
		backupName,
		clusterName,
		redisv1.BackupTargetPreferReplica,
		redisv1.BackupMethodRDB,
		garageInfo,
		"e2e/replica",
	)

	completed := reconcileBackupUntilCompleted(ctx, t, env, operatorBackupNamespace, backupName)
	require.Equal(t, redisv1.BackupPhaseCompleted, completed.Status.Phase)
	require.Equal(t, replicaPodName, completed.Status.TargetPod)
	require.NotEmpty(t, completed.Status.BackupPath)

	require.NoError(t, primary.Terminate(ctx))
	require.NoError(t, replica.Terminate(ctx))

	restoredDataDir := mustTempDir(t, "redis-operator-replica-restored-")
	restoreClusterName := newUniqueName("replica-restored")
	createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, restoreClusterName, 1, backupSecretName)
	require.NoError(t, restorecmd.Run(ctx, restoreClusterName, backupName, operatorBackupNamespace, restoredDataDir))

	restored := startRedisWithDataDir(ctx, t, restoredDataDir, persistenceRDB)
	restoredClient := newRedisClient(ctx, t, restored, "", "")
	waitForRedisReady(t, ctx, restoredClient)
	assertDatasetEventually(t, ctx, restoredClient, "operator-replica-cycle", backupRestoreKeyCount)
	assertDBSizeEventually(t, ctx, restoredClient, backupRestoreKeyCount)
}

func TestDualRestoreFromSameBackupThroughOperatorReconciler(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	env := newOperatorBackupEnvironment(ctx, t)
	configureRestoreKubeconfig(t, env.cfg)

	garageInfo := startGarageContainer(ctx, t)
	backupSecretName := newUniqueName("backup-creds")
	createBackupCredentialsSecret(ctx, t, env.k8sClient, operatorBackupNamespace, backupSecretName, garageInfo)

	sourceDataDir := mustTempDir(t, "redis-operator-dual-source-")
	restoreADataDir := mustTempDir(t, "redis-operator-dual-restore-a-")
	restoreBDataDir := mustTempDir(t, "redis-operator-dual-restore-b-")

	source := startRedisWithDataDir(ctx, t, sourceDataDir, persistenceRDB)
	sourceClient := newRedisClient(ctx, t, source, "", "")
	waitForRedisReady(t, ctx, sourceClient)
	writeDataset(t, ctx, sourceClient, "operator-dual-cycle", backupRestoreKeyCount)

	clusterName := newUniqueName("dual-source")
	primaryPodName := clusterName + "-0"
	createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, 1, backupSecretName)
	markRedisClusterHealthy(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, primaryPodName)
	createClusterPod(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, primaryPodName, "127.0.0.1")

	startBackupAPIServer(ctx, t, backupEndpointSource{
		container:   source,
		redisClient: sourceClient,
		garageInfo:  garageInfo,
	})

	backupName := newUniqueName("backup-dual")
	createRedisBackup(
		ctx,
		t,
		env.k8sClient,
		operatorBackupNamespace,
		backupName,
		clusterName,
		redisv1.BackupTargetPrimary,
		redisv1.BackupMethodRDB,
		garageInfo,
		"e2e/dual",
	)

	completed := reconcileBackupUntilCompleted(ctx, t, env, operatorBackupNamespace, backupName)
	require.Equal(t, redisv1.BackupPhaseCompleted, completed.Status.Phase)
	require.NotEmpty(t, completed.Status.BackupPath)
	require.NoError(t, source.Terminate(ctx))

	restoreAClusterName := newUniqueName("dual-restore-a")
	restoreBClusterName := newUniqueName("dual-restore-b")
	createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, restoreAClusterName, 1, backupSecretName)
	createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, restoreBClusterName, 1, backupSecretName)

	require.NoError(t, restorecmd.Run(ctx, restoreAClusterName, backupName, operatorBackupNamespace, restoreADataDir))
	require.NoError(t, restorecmd.Run(ctx, restoreBClusterName, backupName, operatorBackupNamespace, restoreBDataDir))

	restoreA := startRedisWithDataDir(ctx, t, restoreADataDir, persistenceRDB)
	restoreB := startRedisWithDataDir(ctx, t, restoreBDataDir, persistenceRDB)
	clientA := newRedisClient(ctx, t, restoreA, "", "")
	clientB := newRedisClient(ctx, t, restoreB, "", "")
	waitForRedisReady(t, ctx, clientA)
	waitForRedisReady(t, ctx, clientB)

	assertDatasetEventually(t, ctx, clientA, "operator-dual-cycle", backupRestoreKeyCount)
	assertDatasetEventually(t, ctx, clientB, "operator-dual-cycle", backupRestoreKeyCount)
	assertDBSizeEventually(t, ctx, clientA, backupRestoreKeyCount)
	assertDBSizeEventually(t, ctx, clientB, backupRestoreKeyCount)
}

func TestScheduledBackupRestoreThroughOperatorReconcilers(t *testing.T) {
	requireIntegrationDocker(t)
	ctx := context.Background()

	env := newOperatorBackupEnvironment(ctx, t)
	configureRestoreKubeconfig(t, env.cfg)

	garageInfo := startGarageContainer(ctx, t)
	backupSecretName := newUniqueName("backup-creds")
	createBackupCredentialsSecret(ctx, t, env.k8sClient, operatorBackupNamespace, backupSecretName, garageInfo)

	sourceDataDir := mustTempDir(t, "redis-operator-scheduled-source-")
	restoredDataDir := mustTempDir(t, "redis-operator-scheduled-restored-")
	source := startRedisWithDataDir(ctx, t, sourceDataDir, persistenceRDB)
	sourceClient := newRedisClient(ctx, t, source, "", "")
	waitForRedisReady(t, ctx, sourceClient)
	writeDataset(t, ctx, sourceClient, "operator-scheduled-cycle", backupRestoreKeyCount)

	clusterName := newUniqueName("scheduled-source")
	primaryPodName := clusterName + "-0"
	createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, 1, backupSecretName)
	markRedisClusterHealthy(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, primaryPodName)
	createClusterPod(ctx, t, env.k8sClient, operatorBackupNamespace, clusterName, primaryPodName, "127.0.0.1")

	startBackupAPIServer(ctx, t, backupEndpointSource{
		container:   source,
		redisClient: sourceClient,
		garageInfo:  garageInfo,
	})

	scheduledName := newUniqueName("scheduled")
	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      scheduledName,
			Namespace: operatorBackupNamespace,
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "* * * * *",
			ClusterName: clusterName,
			Target:      redisv1.BackupTargetPrimary,
			Method:      redisv1.BackupMethodRDB,
			Destination: &redisv1.BackupDestination{
				S3: &redisv1.S3Destination{
					Bucket:   garageInfo.Bucket,
					Path:     "e2e/scheduled",
					Endpoint: garageInfo.Endpoint,
					Region:   garageInfo.Region,
				},
			},
		},
	}
	require.NoError(t, env.k8sClient.Create(ctx, scheduled))

	reconcileScheduledBackupOnce(ctx, t, env, operatorBackupNamespace, scheduledName)
	backupName := waitForScheduledBackupName(ctx, t, env.k8sClient, operatorBackupNamespace, scheduledName)

	completed := reconcileBackupUntilCompleted(ctx, t, env, operatorBackupNamespace, backupName)
	require.Equal(t, redisv1.BackupPhaseCompleted, completed.Status.Phase)
	require.NotEmpty(t, completed.Status.BackupPath)

	require.NoError(t, source.Terminate(ctx))

	restoreClusterName := newUniqueName("scheduled-restored")
	createRedisCluster(ctx, t, env.k8sClient, operatorBackupNamespace, restoreClusterName, 1, backupSecretName)
	require.NoError(t, restorecmd.Run(ctx, restoreClusterName, backupName, operatorBackupNamespace, restoredDataDir))

	restored := startRedisWithDataDir(ctx, t, restoredDataDir, persistenceRDB)
	restoredClient := newRedisClient(ctx, t, restored, "", "")
	waitForRedisReady(t, ctx, restoredClient)
	assertDatasetEventually(t, ctx, restoredClient, "operator-scheduled-cycle", backupRestoreKeyCount)
	assertDBSizeEventually(t, ctx, restoredClient, backupRestoreKeyCount)
}

func newOperatorBackupEnvironment(ctx context.Context, t *testing.T) *operatorBackupEnvironment {
	t.Helper()

	scheme := k8sruntime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(redisv1.AddToScheme(scheme))

	_, thisFile, _, ok := goruntime.Caller(0)
	require.True(t, ok, "failed to resolve current file path")
	repoRoot := filepath.Join(filepath.Dir(thisFile), "..", "..")

	testEnv := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join(repoRoot, "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := testEnv.Start()
	if err != nil {
		if os.IsNotExist(err) || isEnvtestBinaryMissing(err) {
			t.Skipf("envtest binaries not found; run 'make setup-envtest' first: %v", err)
		}
		require.NoError(t, err)
	}

	t.Cleanup(func() {
		if cfg != nil {
			require.NoError(t, testEnv.Stop())
		}
	})

	k8sClient, err := ctrlclient.New(cfg, ctrlclient.Options{Scheme: scheme})
	require.NoError(t, err)

	recorder := record.NewFakeRecorder(200)
	backupReconciler := backupcontroller.NewBackupReconciler(k8sClient, scheme, recorder)
	scheduledBackupReconciler := backupcontroller.NewScheduledBackupReconciler(k8sClient, scheme, recorder)

	return &operatorBackupEnvironment{
		cfg:                       cfg,
		k8sClient:                 k8sClient,
		backupReconciler:          backupReconciler,
		scheduledBackupReconciler: scheduledBackupReconciler,
	}
}

func configureRestoreKubeconfig(t *testing.T, cfg *rest.Config) {
	t.Helper()
	kubeconfigPath := writeKubeconfigFile(t, cfg)
	t.Setenv("KUBECONFIG", kubeconfigPath)
	t.Setenv("KUBERNETES_SERVICE_HOST", "")
	t.Setenv("KUBERNETES_SERVICE_PORT", "")
}

func writeKubeconfigFile(t *testing.T, cfg *rest.Config) string {
	t.Helper()

	kubeconfig := clientcmdapi.NewConfig()
	clusterName := "envtest"
	authInfoName := "envtest-user"
	contextName := "envtest-context"

	kubeconfig.Clusters[clusterName] = &clientcmdapi.Cluster{
		Server:                   cfg.Host,
		CertificateAuthorityData: cfg.CAData,
		CertificateAuthority:     cfg.CAFile,
		InsecureSkipTLSVerify:    cfg.Insecure,
	}
	kubeconfig.AuthInfos[authInfoName] = &clientcmdapi.AuthInfo{
		ClientCertificateData: cfg.CertData,
		ClientCertificate:     cfg.CertFile,
		ClientKeyData:         cfg.KeyData,
		ClientKey:             cfg.KeyFile,
		Token:                 cfg.BearerToken,
		Username:              cfg.Username,
		Password:              cfg.Password,
	}
	kubeconfig.Contexts[contextName] = &clientcmdapi.Context{
		Cluster:  clusterName,
		AuthInfo: authInfoName,
	}
	kubeconfig.CurrentContext = contextName

	rawConfig, err := clientcmd.Write(*kubeconfig)
	require.NoError(t, err)

	kubeconfigPath := filepath.Join(t.TempDir(), "kubeconfig")
	require.NoError(t, os.WriteFile(kubeconfigPath, rawConfig, 0o600))

	return kubeconfigPath
}

func createBackupCredentialsSecret(
	ctx context.Context,
	t *testing.T,
	k8sClient ctrlclient.Client,
	namespace, name string,
	garageInfo GarageInfo,
) {
	t.Helper()

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Type: corev1.SecretTypeOpaque,
		Data: map[string][]byte{
			"AWS_ACCESS_KEY_ID":     []byte(garageInfo.AccessKeyID),
			"AWS_SECRET_ACCESS_KEY": []byte(garageInfo.SecretAccessKey),
		},
	}
	require.NoError(t, k8sClient.Create(ctx, secret))
}

func createRedisCluster(
	ctx context.Context,
	t *testing.T,
	k8sClient ctrlclient.Client,
	namespace, name string,
	instances int32,
	backupSecretName string,
) {
	t.Helper()

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: redisv1.RedisClusterSpec{
			Instances: instances,
			Storage: redisv1.StorageSpec{
				Size: resource.MustParse("1Gi"),
			},
			BackupCredentialsSecret: &redisv1.LocalObjectReference{
				Name: backupSecretName,
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, cluster))
}

func markRedisClusterHealthy(
	ctx context.Context,
	t *testing.T,
	k8sClient ctrlclient.Client,
	namespace, name, currentPrimary string,
) {
	t.Helper()

	var cluster redisv1.RedisCluster
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: name, Namespace: namespace}, &cluster))

	patch := ctrlclient.MergeFrom(cluster.DeepCopy())
	cluster.Status.Phase = redisv1.ClusterPhaseHealthy
	cluster.Status.CurrentPrimary = currentPrimary
	require.NoError(t, k8sClient.Status().Patch(ctx, &cluster, patch))
}

func createClusterPod(
	ctx context.Context,
	t *testing.T,
	k8sClient ctrlclient.Client,
	namespace, clusterName, podName, podIP string,
) {
	t.Helper()

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: namespace,
			Labels: map[string]string{
				redisv1.LabelCluster:  clusterName,
				redisv1.LabelWorkload: redisv1.LabelWorkloadData,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{
					Name:  "redis",
					Image: redisImage,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, pod))

	if strings.TrimSpace(podIP) == "" {
		return
	}

	var storedPod corev1.Pod
	require.NoError(t, k8sClient.Get(ctx, types.NamespacedName{Name: podName, Namespace: namespace}, &storedPod))
	patch := ctrlclient.MergeFrom(storedPod.DeepCopy())
	storedPod.Status.PodIP = podIP
	storedPod.Status.Phase = corev1.PodRunning
	require.NoError(t, k8sClient.Status().Patch(ctx, &storedPod, patch))
}

func createRedisBackup(
	ctx context.Context,
	t *testing.T,
	k8sClient ctrlclient.Client,
	namespace, backupName, clusterName string,
	target redisv1.BackupTarget,
	method redisv1.BackupMethod,
	garageInfo GarageInfo,
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
				S3: &redisv1.S3Destination{
					Bucket:   garageInfo.Bucket,
					Path:     pathPrefix,
					Endpoint: garageInfo.Endpoint,
					Region:   garageInfo.Region,
				},
			},
		},
	}
	require.NoError(t, k8sClient.Create(ctx, backup))
}

func reconcileBackupUntilCompleted(
	ctx context.Context,
	t *testing.T,
	env *operatorBackupEnvironment,
	namespace, backupName string,
) redisv1.RedisBackup {
	t.Helper()

	target := types.NamespacedName{Name: backupName, Namespace: namespace}
	deadline := time.Now().Add(backupPollTimeout)

	for time.Now().Before(deadline) {
		_, err := env.backupReconciler.Reconcile(ctx, reconcile.Request{NamespacedName: target})
		require.NoError(t, err)

		var backup redisv1.RedisBackup
		require.NoError(t, env.k8sClient.Get(ctx, target, &backup))

		switch backup.Status.Phase {
		case redisv1.BackupPhaseCompleted:
			return backup
		case redisv1.BackupPhaseFailed:
			require.FailNowf(t, "backup failed", "backup %s/%s failed: %s", namespace, backupName, backup.Status.Error)
		}

		time.Sleep(backupPollInterval)
	}

	require.FailNowf(t, "backup timeout", "timed out waiting for backup %s/%s to complete", namespace, backupName)
	return redisv1.RedisBackup{}
}

func reconcileScheduledBackupOnce(
	ctx context.Context,
	t *testing.T,
	env *operatorBackupEnvironment,
	namespace, scheduledName string,
) {
	t.Helper()

	_, err := env.scheduledBackupReconciler.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: scheduledName, Namespace: namespace},
	})
	require.NoError(t, err)
}

func waitForScheduledBackupName(
	ctx context.Context,
	t *testing.T,
	k8sClient ctrlclient.Client,
	namespace, scheduledName string,
) string {
	t.Helper()

	target := types.NamespacedName{Name: scheduledName, Namespace: namespace}
	deadline := time.Now().Add(backupPollTimeout)

	for time.Now().Before(deadline) {
		var scheduled redisv1.RedisScheduledBackup
		require.NoError(t, k8sClient.Get(ctx, target, &scheduled))
		if scheduled.Status.LastBackupName != "" {
			return scheduled.Status.LastBackupName
		}
		time.Sleep(backupPollInterval)
	}

	require.FailNowf(t, "scheduled backup timeout", "timed out waiting for RedisScheduledBackup %s/%s to create a RedisBackup", namespace, scheduledName)
	return ""
}

func startBackupAPIServer(ctx context.Context, t *testing.T, source backupEndpointSource) {
	t.Helper()

	listener, err := net.Listen("tcp", backupAPIServerAddr)
	if err != nil {
		t.Skipf("cannot bind mock backup API on %s: %v", backupAPIServerAddr, err)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/v1/backup", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		var request backupAPIRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			http.Error(w, fmt.Sprintf("invalid request body: %v", err), http.StatusBadRequest)
			return
		}
		if request.BackupName == "" {
			http.Error(w, "backupName is required", http.StatusBadRequest)
			return
		}
		if request.Destination == nil || request.Destination.S3 == nil || request.Destination.S3.Bucket == "" {
			http.Error(w, "destination.s3.bucket is required", http.StatusBadRequest)
			return
		}

		method := request.Method
		if method == "" {
			method = redisv1.BackupMethodRDB
		}

		response, err := buildBackupArtifactResponse(r.Context(), source, request.BackupName, method, request.Destination.S3)
		if err != nil {
			http.Error(w, fmt.Sprintf("failed to build backup artifact: %v", err), http.StatusInternalServerError)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	})

	server := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- server.Serve(listener)
	}()

	t.Cleanup(func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
		_ = listener.Close()
	})

	select {
	case err := <-serverErrCh:
		if err != nil && err != http.ErrServerClosed {
			require.NoError(t, err)
		}
	case <-time.After(100 * time.Millisecond):
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
}

func buildBackupArtifactResponse(
	ctx context.Context,
	source backupEndpointSource,
	backupName string,
	method redisv1.BackupMethod,
	destination *redisv1.S3Destination,
) (*backupAPIResponse, error) {
	if destination == nil || destination.Bucket == "" {
		return nil, fmt.Errorf("destination bucket is required")
	}

	var artifactType redisv1.BackupArtifactType
	var localArtifactPath string
	var cleanupPaths []string

	switch method {
	case redisv1.BackupMethodRDB:
		if err := triggerAndWaitBGSAVEForBackupAPI(ctx, source.redisClient); err != nil {
			return nil, err
		}
		localArtifactPath = filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.rdb", backupName, time.Now().UnixNano()))
		if err := copyContainerFileFromContainer(ctx, source.container, "/data/dump.rdb", localArtifactPath); err != nil {
			return nil, err
		}
		artifactType = redisv1.BackupArtifactTypeRDB
		cleanupPaths = append(cleanupPaths, localArtifactPath)
	case redisv1.BackupMethodAOF:
		if err := triggerAndWaitBGREWRITEAOFForBackupAPI(ctx, source.redisClient); err != nil {
			return nil, err
		}
		containerArchivePath := fmt.Sprintf("/tmp/%s-%d.appendonly.tar.gz", backupName, time.Now().UnixNano())
		if err := createTarArchiveInContainer(ctx, source.container, containerArchivePath, "/data/appendonlydir"); err != nil {
			return nil, err
		}
		localArtifactPath = filepath.Join(os.TempDir(), fmt.Sprintf("%s-%d.aof.tar.gz", backupName, time.Now().UnixNano()))
		if err := copyContainerFileFromContainer(ctx, source.container, containerArchivePath, localArtifactPath); err != nil {
			return nil, err
		}
		if err := normalizeTarGzArchive(localArtifactPath); err != nil {
			return nil, fmt.Errorf("normalizing AOF archive %s: %w", localArtifactPath, err)
		}
		_, _, _ = source.container.Exec(ctx, []string{"rm", "-f", containerArchivePath})
		artifactType = redisv1.BackupArtifactTypeAOFArchive
		cleanupPaths = append(cleanupPaths, localArtifactPath)
	default:
		return nil, fmt.Errorf("unsupported backup method %q", method)
	}

	defer func() {
		for _, path := range cleanupPaths {
			_ = os.Remove(path)
		}
	}()

	objectKey := buildBackupObjectKey(destination.Path, backupName, artifactType)
	uploadTarget := source.garageInfo
	uploadTarget.Bucket = destination.Bucket
	if destination.Endpoint != "" {
		uploadTarget.Endpoint = destination.Endpoint
	}
	if destination.Region != "" {
		uploadTarget.Region = destination.Region
	}
	if err := uploadFileToS3(ctx, uploadTarget, objectKey, localArtifactPath); err != nil {
		return nil, err
	}

	info, err := os.Stat(localArtifactPath)
	if err != nil {
		return nil, fmt.Errorf("stat backup artifact %s: %w", localArtifactPath, err)
	}

	return &backupAPIResponse{
		ArtifactType: artifactType,
		BackupPath:   fmt.Sprintf("s3://%s/%s", destination.Bucket, objectKey),
		BackupSize:   info.Size(),
	}, nil
}

func buildBackupObjectKey(pathPrefix, backupName string, artifactType redisv1.BackupArtifactType) string {
	extension := ".rdb"
	if artifactType == redisv1.BackupArtifactTypeAOFArchive {
		extension = ".aof.tar.gz"
	}

	objectName := backupName + extension
	trimmedPrefix := strings.Trim(pathPrefix, "/")
	if trimmedPrefix == "" {
		return objectName
	}
	return trimmedPrefix + "/" + objectName
}

func copyContainerFileFromContainer(ctx context.Context, container testcontainers.Container, containerPath, hostPath string) error {
	reader, err := container.CopyFileFromContainer(ctx, containerPath)
	if err != nil {
		return fmt.Errorf("copying %s from container: %w", containerPath, err)
	}
	defer reader.Close() //nolint:errcheck // read-only close is best effort

	if err := os.MkdirAll(filepath.Dir(hostPath), 0o755); err != nil {
		return fmt.Errorf("creating host directory for %s: %w", hostPath, err)
	}

	file, err := os.OpenFile(hostPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("opening host file %s: %w", hostPath, err)
	}
	defer file.Close() //nolint:errcheck // write close error is handled by copy

	if _, err := io.Copy(file, reader); err != nil {
		return fmt.Errorf("copying container file %s to host path %s: %w", containerPath, hostPath, err)
	}

	return nil
}

func createTarArchiveInContainer(ctx context.Context, container testcontainers.Container, archivePath, sourceDir string) error {
	exitCode, output, err := container.Exec(ctx, []string{"tar", "-czf", archivePath, "-C", sourceDir, "."})
	if err != nil {
		return fmt.Errorf("executing tar in container: %w", err)
	}
	outputBytes, err := io.ReadAll(output)
	if err != nil {
		return fmt.Errorf("reading tar output: %w", err)
	}
	if exitCode != 0 {
		return fmt.Errorf("tar command failed with exit code %d: %s", exitCode, strings.TrimSpace(string(outputBytes)))
	}
	return nil
}

func normalizeTarGzArchive(archivePath string) error {
	input, err := os.Open(archivePath)
	if err != nil {
		return fmt.Errorf("opening archive %s: %w", archivePath, err)
	}
	defer input.Close() //nolint:errcheck // read-only close is best effort

	gzipReader, err := gzip.NewReader(input)
	if err != nil {
		return fmt.Errorf("creating gzip reader for %s: %w", archivePath, err)
	}
	defer gzipReader.Close() //nolint:errcheck // close best effort in helper

	tempPath := archivePath + ".normalized"
	output, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("opening normalized archive %s: %w", tempPath, err)
	}

	gzipWriter := gzip.NewWriter(output)
	tarWriter := tar.NewWriter(gzipWriter)
	tarReader := tar.NewReader(gzipReader)

	writeErr := func(innerErr error) error {
		_ = tarWriter.Close()
		_ = gzipWriter.Close()
		_ = output.Close()
		_ = os.Remove(tempPath)
		return innerErr
	}

	for {
		header, err := tarReader.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return writeErr(fmt.Errorf("reading archive entry: %w", err))
		}

		cleanName := strings.TrimPrefix(strings.TrimSpace(header.Name), "./")
		if cleanName == "" || cleanName == "." {
			continue
		}

		switch header.Typeflag {
		case tar.TypeReg, tar.TypeRegA, tar.TypeDir:
			cloned := *header
			cloned.Name = cleanName
			if err := tarWriter.WriteHeader(&cloned); err != nil {
				return writeErr(fmt.Errorf("writing normalized tar header for %s: %w", cleanName, err))
			}
			if header.Typeflag == tar.TypeReg || header.Typeflag == tar.TypeRegA {
				if _, err := io.Copy(tarWriter, tarReader); err != nil {
					return writeErr(fmt.Errorf("copying normalized tar payload for %s: %w", cleanName, err))
				}
			}
		case tar.TypeXHeader, tar.TypeXGlobalHeader:
			// Skip PAX metadata entries from raw `tar -C dir .` output.
			continue
		default:
			return writeErr(fmt.Errorf("unsupported tar entry type %d for %s", header.Typeflag, header.Name))
		}
	}

	if err := tarWriter.Close(); err != nil {
		return writeErr(fmt.Errorf("closing normalized tar writer: %w", err))
	}
	if err := gzipWriter.Close(); err != nil {
		return writeErr(fmt.Errorf("closing normalized gzip writer: %w", err))
	}
	if err := output.Close(); err != nil {
		return writeErr(fmt.Errorf("closing normalized archive %s: %w", tempPath, err))
	}
	if err := os.Rename(tempPath, archivePath); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("replacing archive %s with normalized archive: %w", archivePath, err)
	}

	return nil
}

func triggerAndWaitBGSAVEForBackupAPI(ctx context.Context, redisClient *redis.Client) error {
	if err := redisClient.BgSave(ctx).Err(); err != nil {
		return fmt.Errorf("triggering BGSAVE: %w", err)
	}
	return waitForPersistence(ctx, redisClient, "rdb_bgsave_in_progress:0", "rdb_last_bgsave_status:ok")
}

func triggerAndWaitBGREWRITEAOFForBackupAPI(ctx context.Context, redisClient *redis.Client) error {
	if err := redisClient.Do(ctx, "BGREWRITEAOF").Err(); err != nil {
		return fmt.Errorf("triggering BGREWRITEAOF: %w", err)
	}
	return waitForPersistence(ctx, redisClient, "aof_rewrite_in_progress:0", "aof_last_bgrewrite_status:ok")
}

func waitForPersistence(ctx context.Context, redisClient *redis.Client, completedField, statusField string) error {
	deadline := time.Now().Add(backupPollTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		info, err := redisClient.Info(ctx, "persistence").Result()
		if err != nil {
			return fmt.Errorf("reading INFO persistence: %w", err)
		}
		if strings.Contains(info, completedField) && strings.Contains(info, statusField) {
			return nil
		}

		time.Sleep(backupPollInterval)
	}
	return fmt.Errorf("timed out waiting for Redis persistence completion")
}

func isEnvtestBinaryMissing(err error) bool {
	if err == nil {
		return false
	}
	message := err.Error()
	return strings.Contains(message, "no such file") ||
		strings.Contains(message, "binary") ||
		strings.Contains(message, "kube-apiserver") ||
		strings.Contains(message, "etcd") ||
		strings.Contains(message, "KUBEBUILDER_ASSETS")
}

func newUniqueName(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

func writeDataset(t *testing.T, ctx context.Context, client *redis.Client, keyPrefix string, count int) {
	t.Helper()

	pipeline := client.Pipeline()
	for i := 0; i < count; i++ {
		key := datasetKey(keyPrefix, i)
		value := datasetValue(i)
		pipeline.Set(ctx, key, value, 0)
	}
	_, err := pipeline.Exec(ctx)
	require.NoError(t, err)
}

func assertDatasetEventually(t *testing.T, ctx context.Context, client *redis.Client, keyPrefix string, count int) {
	t.Helper()
	require.Eventually(t, func() bool {
		keys := make([]string, 0, count)
		for i := 0; i < count; i++ {
			keys = append(keys, datasetKey(keyPrefix, i))
		}

		values, err := client.MGet(ctx, keys...).Result()
		if err != nil {
			return false
		}
		if len(values) != count {
			return false
		}
		for i := 0; i < count; i++ {
			got, ok := values[i].(string)
			if !ok || got != datasetValue(i) {
				return false
			}
		}

		return true
	}, 45*time.Second, 250*time.Millisecond)
}

func assertDBSizeEventually(t *testing.T, ctx context.Context, client *redis.Client, expected int) {
	t.Helper()
	require.Eventually(t, func() bool {
		size, err := client.DBSize(ctx).Result()
		return err == nil && int(size) == expected
	}, 45*time.Second, 250*time.Millisecond)
}

func containsRedisInfoField(infoOutput, field string) bool {
	return len(infoOutput) > 0 && strings.Contains(infoOutput, field)
}

func datasetKey(prefix string, idx int) string {
	return fmt.Sprintf("%s:%04d", prefix, idx)
}

func datasetValue(idx int) string {
	return fmt.Sprintf("value-%04d", idx)
}
