package v1

import (
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
)

func int32Ptr(v int32) *int32 { return &v }

func quantityPtr(s string) *resource.Quantity {
	q := resource.MustParse(s)
	return &q
}

func specWithLimit(limit string, mem *MemorySpec) RedisClusterSpec {
	spec := RedisClusterSpec{Memory: mem}
	if limit != "" {
		spec.Resources.Limits = corev1.ResourceList{
			corev1.ResourceMemory: resource.MustParse(limit),
		}
	}
	return spec
}

func TestResolveMaxMemoryBytes(t *testing.T) {
	tests := []struct {
		name      string
		spec      RedisClusterSpec
		wantBytes int64
		wantOK    bool
	}{
		{
			name:   "no memory spec",
			spec:   specWithLimit("1Gi", nil),
			wantOK: false,
		},
		{
			name:      "explicit maxMemory",
			spec:      specWithLimit("", &MemorySpec{MaxMemory: quantityPtr("256Mi")}),
			wantBytes: 256 * 1024 * 1024,
			wantOK:    true,
		},
		{
			name:   "explicit zero maxMemory is not configured",
			spec:   specWithLimit("", &MemorySpec{MaxMemory: quantityPtr("0")}),
			wantOK: false,
		},
		{
			name:      "percent of limit",
			spec:      specWithLimit("1Gi", &MemorySpec{MaxMemoryPercent: int32Ptr(75)}),
			wantBytes: 1024 * 1024 * 1024 * 75 / 100,
			wantOK:    true,
		},
		{
			name:   "percent without limit",
			spec:   specWithLimit("", &MemorySpec{MaxMemoryPercent: int32Ptr(75)}),
			wantOK: false,
		},
		{
			name:      "percent 100 equals limit",
			spec:      specWithLimit("512Mi", &MemorySpec{MaxMemoryPercent: int32Ptr(100)}),
			wantBytes: 512 * 1024 * 1024,
			wantOK:    true,
		},
		{
			name:   "empty memory spec resolves nothing",
			spec:   specWithLimit("1Gi", &MemorySpec{}),
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bytes, ok := tt.spec.ResolveMaxMemoryBytes()
			if ok != tt.wantOK {
				t.Fatalf("ResolveMaxMemoryBytes() ok = %v, want %v", ok, tt.wantOK)
			}
			if ok && bytes != tt.wantBytes {
				t.Errorf("ResolveMaxMemoryBytes() bytes = %d, want %d", bytes, tt.wantBytes)
			}
		})
	}
}

func TestMemoryLimitBytes(t *testing.T) {
	if got := specWithLimit("", nil).MemoryLimitBytes(); got != 0 {
		t.Errorf("MemoryLimitBytes() with no limit = %d, want 0", got)
	}
	if got := specWithLimit("128Mi", nil).MemoryLimitBytes(); got != 128*1024*1024 {
		t.Errorf("MemoryLimitBytes() = %d, want %d", got, 128*1024*1024)
	}
}

func TestEffectiveMaxMemoryPolicy(t *testing.T) {
	if got := (RedisClusterSpec{}).EffectiveMaxMemoryPolicy(); got != "" {
		t.Errorf("EffectiveMaxMemoryPolicy() with no memory = %q, want empty", got)
	}
	spec := RedisClusterSpec{Memory: &MemorySpec{MaxMemoryPolicy: MaxMemoryPolicyNoEviction}}
	if got := spec.EffectiveMaxMemoryPolicy(); got != "noeviction" {
		t.Errorf("EffectiveMaxMemoryPolicy() = %q, want noeviction", got)
	}
}
