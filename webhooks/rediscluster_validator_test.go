package webhooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
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

func validClusterModeSpec() *redisv1.RedisCluster {
	cluster := validCluster()
	cluster.Spec.Mode = redisv1.ClusterModeCluster
	cluster.Spec.Instances = 0
	cluster.Spec.Shards = 3
	cluster.Spec.ReplicasPerShard = 1
	cluster.Spec.MinSyncReplicas = 0
	cluster.Spec.MaxSyncReplicas = 0
	cluster.Spec.ReplicaMode = nil
	return cluster
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
	require.NoError(t, clientgoscheme.AddToScheme(scheme))
	require.NoError(t, redisv1.AddToScheme(scheme))

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objects...).Build()
	return &RedisClusterValidator{Reader: fakeClient}
}

func boolPtr(v bool) *bool {
	return &v
}

func TestValidateCreate_ValidCluster(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_ConnectionSecretValidName(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.ConnectionSecret = &redisv1.ConnectionSecretSpec{Name: "my-app-redis"}

	_, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
}

func TestValidateCreate_ConnectionSecretInvalidName(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.ConnectionSecret = &redisv1.ConnectionSecretSpec{Name: "Bad_Name"}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "connectionSecret")
}

func TestValidateCreate_ConnectionSecretEmptyName(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.ConnectionSecret = &redisv1.ConnectionSecretSpec{Name: "  "}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "name is required")
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

func TestValidateCreate_ClusterModeValid(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validClusterModeSpec()

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_ClusterModeRejectsInstances(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validClusterModeSpec()
	cluster.Spec.Instances = 3

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), clusterInstancesForbiddenMessage)
}

func TestValidateCreate_ClusterModeRequiresThreeShards(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validClusterModeSpec()
	cluster.Spec.Shards = 2

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), clusterShardsMinMessage)
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

func TestValidateCreate_ReplicaModeEnabledRequiresSource(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
	}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), replicaModeSourceRequiredMessage)
}

func TestValidateCreate_ReplicaModeSourceRequiresHost(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Source:  &redisv1.ReplicaSourceSpec{Port: 6379},
	}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), replicaModeHostRequiredMessage)
}

func TestValidateCreate_ReplicaModeSourceRejectsInvalidPort(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: -1,
		},
	}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "must be between 1 and 65535 when set")
}

func TestValidateCreate_ReplicaModeAuthSecretNotFound(t *testing.T) {
	v := validatorWithReader(t)
	cluster := validCluster()
	cluster.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Source: &redisv1.ReplicaSourceSpec{
			Host:           "external-primary",
			Port:           6379,
			AuthSecretName: "missing-secret",
		},
	}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "missing-secret")
}

func TestValidateCreate_ReplicaModeValid(t *testing.T) {
	authSecret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "replica-auth",
			Namespace: "default",
		},
		Data: map[string][]byte{
			"password": []byte("secret"),
		},
	}
	v := validatorWithReader(t, authSecret)
	cluster := validCluster()
	cluster.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Source: &redisv1.ReplicaSourceSpec{
			Host:           "external-primary",
			Port:           6379,
			AuthSecretName: "replica-auth",
		},
	}

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

func TestValidateCreate_MaintenanceSingleInstanceReusePVCFalseRejected(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Instances = 1
	cluster.Spec.NodeMaintenanceWindow = &redisv1.NodeMaintenanceWindow{
		InProgress: true,
		ReusePVC:   boolPtr(false),
	}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), maintenanceSingleInstanceMessage)
}

func TestValidateCreate_MaintenanceSingleInstanceReusePVCTrueAllowed(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := validCluster()
	cluster.Spec.Instances = 1
	cluster.Spec.NodeMaintenanceWindow = &redisv1.NodeMaintenanceWindow{
		InProgress: true,
		ReusePVC:   boolPtr(true),
	}

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

func TestValidateUpdate_StorageSizeIncreaseAllowed(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	new := validCluster()
	new.Spec.Storage.Size = resource.MustParse("10Gi")

	warnings, err := v.ValidateUpdate(context.Background(), old, new)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateUpdate_ReplicaModeDisableRequiresPromote(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	old.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: 6379,
		},
	}
	new := validCluster()
	new.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: false,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: 6379,
		},
	}

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), replicaModeDisableMessage)
}

func TestValidateUpdate_ReplicaModeDisableAllowedAfterPromote(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	old.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Promote: true,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: 6379,
		},
	}
	new := validCluster()
	new.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: false,
		Promote: false,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: 6379,
		},
	}

	warnings, err := v.ValidateUpdate(context.Background(), old, new)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateUpdate_ReplicaModePromoteRequiresPreviouslyEnabled(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	new := validCluster()
	new.Spec.ReplicaMode = &redisv1.ReplicaModeSpec{
		Enabled: true,
		Promote: true,
		Source: &redisv1.ReplicaSourceSpec{
			Host: "external-primary",
			Port: 6379,
		},
	}

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "replicaMode.promote can only be set after replicaMode is already enabled")
}

func TestValidateUpdate_StorageSizeDecreaseRejected(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	old.Spec.Storage.Size = resource.MustParse("10Gi")
	new := validCluster()

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage")
	assert.Contains(t, err.Error(), "decreased")
}

func TestValidateUpdate_StorageSizeIncreaseRejectedForNonExpandableStorageClass(t *testing.T) {
	storageClassName := "no-expand"
	allowVolumeExpansion := false
	storageClass := &storagev1.StorageClass{
		ObjectMeta:           metav1.ObjectMeta{Name: storageClassName},
		AllowVolumeExpansion: &allowVolumeExpansion,
	}

	v := validatorWithReader(t, storageClass)
	old := validCluster()
	old.Spec.Storage.StorageClassName = &storageClassName
	new := validCluster()
	new.Spec.Storage.StorageClassName = &storageClassName
	new.Spec.Storage.Size = resource.MustParse("2Gi")

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "does not allow volume expansion")
}

func TestValidateUpdate_StorageSizeDecreaseAllowsRollbackWhenPVCRequestsNotIncreased(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-data-0",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "test-cluster",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Gi"),
				},
			},
		},
	}

	v := validatorWithReader(t, pvc)
	old := validCluster()
	old.Spec.Storage.Size = resource.MustParse("2Gi")
	new := validCluster()
	new.Spec.Storage.Size = resource.MustParse("1Gi")

	warnings, err := v.ValidateUpdate(context.Background(), old, new)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateUpdate_StorageSizeDecreaseRejectedWhenBelowCurrentPVCRequest(t *testing.T) {
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-cluster-data-0",
			Namespace: "default",
			Labels: map[string]string{
				redisv1.LabelCluster: "test-cluster",
			},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("2Gi"),
				},
			},
		},
	}

	v := validatorWithReader(t, pvc)
	old := validCluster()
	old.Spec.Storage.Size = resource.MustParse("3Gi")
	new := validCluster()
	new.Spec.Storage.Size = resource.MustParse("1Gi")

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "currently requested PVC size")
}

func TestValidateUpdate_StandaloneToSentinelAllowed(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster() // standalone, 3 instances
	new := validCluster()
	new.Spec.Mode = redisv1.ClusterModeSentinel
	new.Status.CurrentPrimary = "test-cluster-0"

	warnings, err := v.ValidateUpdate(context.Background(), old, new)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateUpdate_StandaloneToSentinelScalesInstances(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	old.Spec.Instances = 1
	new := validCluster()
	new.Spec.Mode = redisv1.ClusterModeSentinel
	new.Spec.Instances = 3
	new.Status.CurrentPrimary = "test-cluster-0"

	warnings, err := v.ValidateUpdate(context.Background(), old, new)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateUpdate_StandaloneToSentinelRejectedWithoutPrimary(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	new := validCluster()
	new.Spec.Mode = redisv1.ClusterModeSentinel
	new.Status.CurrentPrimary = ""

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "currentPrimary")
}

func TestValidateUpdate_StandaloneToSentinelRejectsTooFewInstances(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	old.Spec.Instances = 1
	new := validCluster()
	new.Spec.Mode = redisv1.ClusterModeSentinel
	new.Spec.Instances = 1
	new.Status.CurrentPrimary = "test-cluster-0"

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), sentinelInstancesMinMessage)
}

func TestValidateUpdate_StandaloneToSentinelRejectsTLS(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	old.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls"}
	old.Spec.CASecret = &redisv1.LocalObjectReference{Name: "ca"}
	new := validCluster()
	new.Spec.Mode = redisv1.ClusterModeSentinel
	new.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls"}
	new.Spec.CASecret = &redisv1.LocalObjectReference{Name: "ca"}
	new.Status.CurrentPrimary = "test-cluster-0"

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), sentinelTLSUnsupported)
}

func TestValidateUpdate_StandaloneToSentinelRejectsClearingTLS(t *testing.T) {
	// A TLS-enabled standalone cluster that clears TLS in the same patch as the
	// mode flip must still be rejected: the new spec passes the target TLS check,
	// so we must also inspect the old spec's TLS fields.
	v := &RedisClusterValidator{}
	old := validCluster()
	old.Spec.TLSSecret = &redisv1.LocalObjectReference{Name: "tls"}
	old.Spec.CASecret = &redisv1.LocalObjectReference{Name: "ca"}
	new := validCluster()
	new.Spec.Mode = redisv1.ClusterModeSentinel
	new.Spec.TLSSecret = nil
	new.Spec.CASecret = nil
	new.Status.CurrentPrimary = "test-cluster-0"

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), modeMigrationTLSMessage)
}

func TestValidateUpdate_UnsupportedModeTransitionsRejected(t *testing.T) {
	tests := []struct {
		name string
		from redisv1.ClusterMode
		to   redisv1.ClusterMode
	}{
		{"sentinel to standalone", redisv1.ClusterModeSentinel, redisv1.ClusterModeStandalone},
		{"standalone to cluster", redisv1.ClusterModeStandalone, redisv1.ClusterModeCluster},
		{"sentinel to cluster", redisv1.ClusterModeSentinel, redisv1.ClusterModeCluster},
		{"cluster to standalone", redisv1.ClusterModeCluster, redisv1.ClusterModeStandalone},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			v := &RedisClusterValidator{}
			old := validCluster()
			old.Spec.Mode = tt.from
			new := validCluster()
			new.Spec.Mode = tt.to
			new.Status.CurrentPrimary = "test-cluster-0"

			_, err := v.ValidateUpdate(context.Background(), old, new)
			require.Error(t, err)
			assert.Contains(t, err.Error(), "unsupported mode transition")
		})
	}
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
			name: "cluster mode with required fields is valid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.Mode = redisv1.ClusterModeCluster
				c.Spec.Instances = 0
				c.Spec.Shards = 3
				c.Spec.ReplicasPerShard = 1
			},
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
			name: "cluster mode with explicit instances is invalid",
			modify: func(c *redisv1.RedisCluster) {
				c.Spec.Mode = redisv1.ClusterModeCluster
				c.Spec.Instances = 3
				c.Spec.Shards = 3
			},
			wantErr:   true,
			errSubstr: clusterInstancesForbiddenMessage,
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

func TestValidateCreate_BootstrapBackupMissingDestination(t *testing.T) {
	backup := completedBackup("missing-destination", "default")
	backup.Spec.Destination = nil

	v := validatorWithReader(t, backup)
	cluster := validCluster()
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backup.Name}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "invalid destination")
}

func TestValidateCreate_BootstrapBackupAzureDestinationAccepted(t *testing.T) {
	backup := completedBackup("azure-backup", "default")
	backup.Spec.Destination = &redisv1.BackupDestination{
		Azure: &redisv1.AzureBlobDestination{Container: "redis-backups"},
	}
	backup.Status.BackupPath = "azblob://redis-backups/backups/source-cluster.rdb"

	v := validatorWithReader(t, backup)
	cluster := validCluster()
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backup.Name}

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
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

// ephemeralCluster returns a valid standalone cluster backed by emptyDir storage.
func ephemeralCluster() *redisv1.RedisCluster {
	cluster := validCluster()
	cluster.Spec.Storage.Type = redisv1.StorageTypeEmptyDir
	return cluster
}

func TestValidateCreate_EphemeralStorageValid(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := ephemeralCluster()

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_EphemeralSentinelValid(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := ephemeralCluster()
	cluster.Spec.Mode = redisv1.ClusterModeSentinel
	cluster.Spec.Instances = 3

	warnings, err := v.ValidateCreate(context.Background(), cluster)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}

func TestValidateCreate_EphemeralRejectsBootstrap(t *testing.T) {
	backup := completedBackup("seed", "default")
	v := validatorWithReader(t, backup)
	cluster := ephemeralCluster()
	cluster.Spec.Bootstrap = &redisv1.BootstrapSpec{BackupName: backup.Name}

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "bootstrap")
	assert.Contains(t, err.Error(), "ephemeral")
}

func TestValidateCreate_EphemeralRejectsStorageClass(t *testing.T) {
	v := &RedisClusterValidator{}
	cluster := ephemeralCluster()
	className := "fast-ssd"
	cluster.Spec.Storage.StorageClassName = &className

	_, err := v.ValidateCreate(context.Background(), cluster)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storageClassName")
}

func TestValidateUpdate_StorageTypeImmutable(t *testing.T) {
	v := &RedisClusterValidator{}
	old := validCluster()
	old.Spec.Storage.Type = redisv1.StorageTypePVC
	new := ephemeralCluster()

	_, err := v.ValidateUpdate(context.Background(), old, new)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "storage.type")
	assert.Contains(t, err.Error(), "immutable")
}

func TestValidateUpdate_EphemeralSizeDecreaseAllowed(t *testing.T) {
	v := &RedisClusterValidator{}
	old := ephemeralCluster()
	old.Spec.Storage.Size = resource.MustParse("10Gi")
	new := ephemeralCluster()
	new.Spec.Storage.Size = resource.MustParse("1Gi")

	warnings, err := v.ValidateUpdate(context.Background(), old, new)
	assert.NoError(t, err)
	assert.Nil(t, warnings)
}
