package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/resource"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestDataVolumeSource_PVCBacked(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Storage.Type = redisv1.StorageTypePVC

	src := dataVolumeSource(cluster, 0)

	require.NotNil(t, src.PersistentVolumeClaim)
	assert.Nil(t, src.EmptyDir)
	assert.Equal(t, pvcNameForIndex(cluster.Name, 0), src.PersistentVolumeClaim.ClaimName)
}

func TestDataVolumeSource_DefaultsToPVC(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	// Type left empty: the defaulter maps this to pvc, and IsEphemeral() is false.
	cluster.Spec.Storage.Type = ""

	src := dataVolumeSource(cluster, 0)

	require.NotNil(t, src.PersistentVolumeClaim)
	assert.Nil(t, src.EmptyDir)
}

func TestDataVolumeSource_EphemeralWithSizeLimit(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Storage.Type = redisv1.StorageTypeEmptyDir
	cluster.Spec.Storage.Size = resource.MustParse("512Mi")

	src := dataVolumeSource(cluster, 0)

	require.NotNil(t, src.EmptyDir)
	assert.Nil(t, src.PersistentVolumeClaim)
	require.NotNil(t, src.EmptyDir.SizeLimit)
	assert.Equal(t, resource.MustParse("512Mi"), *src.EmptyDir.SizeLimit)
}

func TestDataVolumeSource_EphemeralZeroSizeUnbounded(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Storage.Type = redisv1.StorageTypeEmptyDir
	cluster.Spec.Storage.Size = resource.Quantity{}

	src := dataVolumeSource(cluster, 0)

	require.NotNil(t, src.EmptyDir)
	assert.Nil(t, src.EmptyDir.SizeLimit)
}

func TestComputeSpecHash_EphemeralSizeChangeRollsPods(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Storage.Type = redisv1.StorageTypeEmptyDir
	cluster.Spec.Storage.Size = resource.MustParse("1Gi")

	r, _ := newReconciler(cluster)
	before := r.computeSpecHash(cluster)

	cluster.Spec.Storage.Size = resource.MustParse("2Gi")
	after := r.computeSpecHash(cluster)

	assert.NotEqual(t, before, after)
}

func TestComputeSpecHash_PVCSizeChangeDoesNotRollPods(t *testing.T) {
	cluster := newTestCluster("test", "default", 1)
	cluster.Spec.Storage.Type = redisv1.StorageTypePVC
	cluster.Spec.Storage.Size = resource.MustParse("1Gi")

	r, _ := newReconciler(cluster)
	before := r.computeSpecHash(cluster)

	cluster.Spec.Storage.Size = resource.MustParse("2Gi")
	after := r.computeSpecHash(cluster)

	assert.Equal(t, before, after)
}
