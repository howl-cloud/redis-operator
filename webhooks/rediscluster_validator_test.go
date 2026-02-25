package webhooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// validCluster returns a RedisCluster that passes all validation.
func validCluster() *redisv1.RedisCluster {
	return &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster",
			Namespace: "default",
		},
		Spec: redisv1.RedisClusterSpec{
			Instances: 3,
			Mode:      redisv1.ClusterModeStandalone,
			ImageName: "redis:7.2",
			Storage: redisv1.StorageSpec{
				Size: resource.MustParse("1Gi"),
			},
			MinSyncReplicas: 0,
			MaxSyncReplicas: 0,
		},
	}
}

func completedBackup(name, namespace string) *redisv1.RedisBackup {
	return &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: "source-cluster",
			Method:      redisv1.BackupMethodRDB,
			Destination: &redisv1.BackupDestination{
				S3: &redisv1.S3Destination{
					Bucket: "test-bucket",
				},
			},
		},
		Status: redisv1.RedisBackupStatus{
			Phase:      redisv1.BackupPhaseCompleted,
			BackupPath: "s3://test-bucket/backups/source-cluster.rdb",
		},
	}
}

func validatorWithReader(t *testing.T, objects ...client.Object) *RedisClusterValidator {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, redisv1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &RedisClusterValidator{Reader: fakeClient}
}

func TestValidateCreate_ValidCluster(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_InstancesTooLow(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Instances = 0

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "instances")
	assert.Contains(t, err.Error(), "must be at least 1")
}

func TestValidateCreate_MinSyncReplicasTooHigh(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Instances = 3
	cluster.Spec.MinSyncReplicas = 3 // exceeds instances-1 (2)

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "minSyncReplicas")
}

func TestValidateCreate_MaxSyncLessThanMin(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.MinSyncReplicas = 2
	cluster.Spec.MaxSyncReplicas = 1

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "maxSyncReplicas")
}

func TestValidateCreate_UnsupportedMode(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Mode = redisv1.ClusterModeCluster

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), unsupportedModeMessage)
}

func TestValidateCreate_SentinelModeRequiresThreeInstances(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	cluster.Spec.Instances = 2

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), sentinelInstancesMinMessage)
}

func TestValidateCreate_SentinelModeWithThreeInstances(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	cluster.Spec.Instances = 3

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_SentinelModeRejectsTLS(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	cluster.Spec.Instances = 3
	cluster.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls-secret"}
	cluster.Spec.CASecret = &redisv1.LocalObjectReference{Name: "ca-secret"}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), sentinelTLSUnsupported)
}

func TestValidateCreate_TLSSecretRequiresCASecret(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls-secret"}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), caSecretRequiredMessage)
}

func TestValidateCreate_CASecretRequiresTLSSecret(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.CASecret = &redisv1.LocalObjectReference{Name: "ca-secret"}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), tlsSecretRequiredMessage)
}

func TestValidateCreate_TLSAndCASecretsSet(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls-secret"}
	cluster.Spec.CASecret = &redisv1.LocalObjectReference{Name: "ca-secret"}

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_PrimaryUpdateApprovalAnnotationValid(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Annotations = map[string]string{
		redisv1.AnnotationApprovePrimaryUpdate: "true",
	}

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_PrimaryUpdateApprovalAnnotationInvalid(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Annotations = map[string]string{
		redisv1.AnnotationApprovePrimaryUpdate: "false",
	}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), primaryUpdateApprovalValue)
}

func TestValidateCreate_MultipleErrors(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Instances = 0
	cluster.Spec.MinSyncReplicas = 5
	cluster.Spec.MaxSyncReplicas = 1

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	// Should contain errors for instances, minSyncReplicas, and maxSyncReplicas.
	assert.Contains(t, err.Error(), "instances")
	assert.Contains(t, err.Error(), "minSyncReplicas")
	assert.Contains(t, err.Error(), "maxSyncReplicas")
}

func TestValidateCreate_SingleInstance(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Instances = 1
	cluster.Spec.MinSyncReplicas = 0
	cluster.Spec.MaxSyncReplicas = 0

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_WrongType(t *testing.T) {
	v := &RedisClusterValidator{}
	// Pass a non-RedisCluster object.
	_, err := v.ValidateCreate(context.Background(), &redisv1.RedisBackup{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected RedisCluster")
}

func TestValidateUpdate_Valid(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	new := validCluster()
	new.Spec.Instances = 5 // scaling is allowed

	warnings, err := v.ValidateUpdate(context.Background(), old, new)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateUpdate_StorageSizeImmutable(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	new := validCluster()
	new.Spec.Storage.Size = resource.MustParse("10Gi")

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage")
	assert.Contains(t, err.Error(), "immutable")
}

func TestValidateUpdate_ModeImmutable(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	new := validCluster()
	new.Spec.Mode = redisv1.ClusterModeCluster

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "mode")
	assert.Contains(t, err.Error(), "immutable")
}

func TestValidateUpdate_WrongOldType(t *testing.T) {
	v := &RedisClusterValidator{}
	_, err := v.ValidateUpdate(context.Background(), &redisv1.RedisBackup{}, validCluster())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected RedisCluster")
}

func TestValidateUpdate_WrongNewType(t *testing.T) {
	v := &RedisClusterValidator{}
	_, err := v.ValidateUpdate(context.Background(), validCluster(), &redisv1.RedisBackup{})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "expected RedisCluster")
}

func TestValidateDelete_AlwaysAllowed(t *testing.T) {
	v := &RedisClusterValidator{}
	warnings, err := v.ValidateDelete(context.Background(), validCluster())
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_TableDriven(t *testing.T) {
	tests := []struct {
		name      string
		modify    func(c *redisv1.RedisCluster)
		wantErr   bool
		errSubstr string
	}{
		{
			name:   "valid 3-instance cluster",
			modify: func(_ *redisv1.RedisCluster) {},
		},
		{
			name: "sentinel mode with 3 instances is valid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.Mode = redisv1.ClusterModeSentinel
			},
		},
		{
			name: "cluster mode unsupported",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.Mode = redisv1.ClusterModeCluster
			},
			wantErr:   true,
			errSubstr: unsupportedModeMessage,
		},
		{
			name: "sentinel mode with fewer than 3 instances is invalid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.Mode = redisv1.ClusterModeSentinel
				c.Spec.Instances = 2
			},
			wantErr:   true,
			errSubstr: sentinelInstancesMinMessage,
		},
		{
			name: "sentinel mode with TLS is invalid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.Mode = redisv1.ClusterModeSentinel
				c.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls-secret"}
				c.Spec.CASecret = &redisv1.LocalObjectReference{Name: "ca-secret"}
			},
			wantErr:   true,
			errSubstr: sentinelTLSUnsupported,
		},
		{
			name:      "zero instances",
			modify:    func(c *redisv1.RedisCluster) { c.Spec.Instances = 0 },
			wantErr:   true,
			errSubstr: "instances",
		},
		{
			name: "minSync equals instances-1 is valid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.MinSyncReplicas = 2 // instances-1 = 2
				c.Spec.MaxSyncReplicas = 2
			},
		},
		{
			name: "maxSync equals minSync is valid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.MinSyncReplicas = 1
				c.Spec.MaxSyncReplicas = 1
			},
		},
		{
			name: "maxSync greater than minSync is valid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.MinSyncReplicas = 1
				c.Spec.MaxSyncReplicas = 2
			},
		},
	}

	v := &RedisClusterValidator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := validCluster()
			tt.modify(cluster)
			_, err := v.ValidateCreate(context.Background(), cluster)
			if tt.wantErr {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.errSubstr)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestValidateCreate_BootstrapBackupNotFound(t *testing.T) {
	v := validatorWithReader(t)
	cluster := validCluster()
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: "missing-backup"}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "Not found")
}

func TestValidateCreate_BootstrapBackupIncomplete(t *testing.T) {
	backup := completedBackup("incomplete", "default")
	backup.Status.Phase = redisv1.BackupPhaseRunning

	v := validatorWithReader(t, backup)
	cluster := validCluster()
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backup.Name}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be in phase")
}

func TestValidateCreate_BootstrapBackupMissingS3Destination(t *testing.T) {
	backup := completedBackup("missing-destination", "default")
	backup.Spec.Destination = nil

	v := validatorWithReader(t, backup)
	cluster := validCluster()
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backup.Name}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must define an S3 destination")
}

func TestValidateCreate_BootstrapBackupAOFAccepted(t *testing.T) {
	backup := completedBackup("aof-backup", "default")
	backup.Spec.Method = redisv1.BackupMethodAOF
	backup.Status.BackupPath = "s3://test-bucket/backups/source-cluster.aof.tar.gz"
	backup.Status.ArtifactType = redisv1.BackupArtifactTypeAOFArchive

	v := validatorWithReader(t, backup)
	cluster := validCluster()
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backup.Name}

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_BootstrapBackupCompletedAccepted(t *testing.T) {
	backup := completedBackup("completed", "default")

	v := validatorWithReader(t, backup)
	cluster := validCluster()
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backup.Name}

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateUpdate_BootstrapBackupNameImmutable(t *testing.T) {
	backupOne := completedBackup("backup-one", "default")
	backupTwo := completedBackup("backup-two", "default")

	v := validatorWithReader(t, backupOne, backupTwo)
	oldCluster := validCluster()
	oldCluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backupOne.Name}

	newCluster := validCluster()
	newCluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backupTwo.Name}

	_, err := v.ValidateUpdate(context.Background(), oldCluster, newCluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bootstrap")
	assert.Contains(t, err.Error(), "immutable")
}
