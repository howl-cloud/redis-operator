package cluster

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestPodNameForIndex(t *testing.T) {
	tests := []struct {
		clusterName string
		index       int
		expected    string
	}{
		{"mycluster", 0, "mycluster-0"},
		{"mycluster", 1, "mycluster-1"},
		{"mycluster", 99, "mycluster-99"},
		{"test", 0, "test-0"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, podNameForIndex(tt.clusterName, tt.index))
		})
	}
}

func TestPvcNameForIndex(t *testing.T) {
	tests := []struct {
		clusterName string
		index       int
		expected    string
	}{
		{"mycluster", 0, "mycluster-data-0"},
		{"mycluster", 1, "mycluster-data-1"},
		{"test", 5, "test-data-5"},
	}

	for _, tt := range tests {
		t.Run(tt.expected, func(t *testing.T) {
			assert.Equal(t, tt.expected, pvcNameForIndex(tt.clusterName, tt.index))
		})
	}
}

func TestPodLabels(t *testing.T) {
	labels := podLabels("mycluster", "mycluster-0", "primary")
	assert.Equal(t, "mycluster", labels["redis.io/cluster"])
	assert.Equal(t, "mycluster-0", labels["redis.io/instance"])
	assert.Equal(t, "primary", labels["redis.io/role"])
	assert.Len(t, labels, 3)
}

func TestServiceAccountName(t *testing.T) {
	assert.Equal(t, "mycluster", serviceAccountName("mycluster"))
	assert.Equal(t, "test", serviceAccountName("test"))
}

func TestPdbMinAvailable(t *testing.T) {
	tests := []struct {
		instances int32
		expected  intstr.IntOrString
	}{
		{1, intstr.FromInt32(1)},  // max(1, 1-1) = max(1,0) = 1
		{2, intstr.FromInt32(1)},  // max(1, 2-1) = max(1,1) = 1
		{3, intstr.FromInt32(2)},  // max(1, 3-1) = max(1,2) = 2
		{5, intstr.FromInt32(4)},  // max(1, 5-1) = max(1,4) = 4
		{10, intstr.FromInt32(9)}, // max(1, 10-1) = max(1,9) = 9
	}

	for _, tt := range tests {
		t.Run("", func(t *testing.T) {
			result := pdbMinAvailable(tt.instances)
			assert.Equal(t, tt.expected.IntValue(), result.IntValue())
		})
	}
}

func TestLeaderServiceName(t *testing.T) {
	assert.Equal(t, "mycluster-leader", leaderServiceName("mycluster"))
}

func TestReplicaServiceName(t *testing.T) {
	assert.Equal(t, "mycluster-replica", replicaServiceName("mycluster"))
}

func TestAnyServiceName(t *testing.T) {
	assert.Equal(t, "mycluster-any", anyServiceName("mycluster"))
}

func TestPodIndex(t *testing.T) {
	tests := []struct {
		clusterName string
		podName     string
		expected    int
	}{
		{"mycluster", "mycluster-0", 0},
		{"mycluster", "mycluster-1", 1},
		{"mycluster", "mycluster-99", 99},
		{"test", "test-5", 5},
	}

	for _, tt := range tests {
		t.Run(tt.podName, func(t *testing.T) {
			assert.Equal(t, tt.expected, podIndex(tt.clusterName, tt.podName))
		})
	}
}

func TestIntOrString(t *testing.T) {
	result := intOrString(8080)
	assert.Equal(t, 8080, result.IntValue())

	result = intOrString(6379)
	assert.Equal(t, 6379, result.IntValue())
}
