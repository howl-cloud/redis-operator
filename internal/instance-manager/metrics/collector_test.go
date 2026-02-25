package metrics

import (
	"context"
	"errors"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
	"github.com/redis/go-redis/v9"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestRedisCollector_Collect(t *testing.T) {
	t.Parallel()

	const masterInfo = "# Server\r\n" +
		"redis_version:7.2.0\r\n" +
		"os:Linux 6.8\r\n" +
		"uptime_in_seconds:3600\r\n" +
		"# Clients\r\n" +
		"connected_clients:10\r\n" +
		"blocked_clients:2\r\n" +
		"# Memory\r\n" +
		"used_memory:1048576\r\n" +
		"used_memory_rss:2097152\r\n" +
		"used_memory_peak:3145728\r\n" +
		"maxmemory:4194304\r\n" +
		"mem_fragmentation_ratio:1.25\r\n" +
		"# Replication\r\n" +
		"role:master\r\n" +
		"connected_slaves:2\r\n" +
		"master_repl_offset:2048\r\n" +
		"# Persistence\r\n" +
		"rdb_last_save_time:1700000000\r\n" +
		"rdb_changes_since_last_save:7\r\n" +
		"loading:0\r\n" +
		"# Stats\r\n" +
		"total_commands_processed:42\r\n" +
		"instantaneous_ops_per_sec:88\r\n" +
		"keyspace_hits:100\r\n" +
		"keyspace_misses:5\r\n"

	const replicaInfo = "# Server\r\n" +
		"redis_version:7.2.0\r\n" +
		"os:Linux 6.8\r\n" +
		"uptime_in_seconds:123\r\n" +
		"# Clients\r\n" +
		"connected_clients:4\r\n" +
		"blocked_clients:0\r\n" +
		"# Replication\r\n" +
		"role:slave\r\n" +
		"master_link_status:up\r\n" +
		"master_repl_offset:3000\r\n" +
		"slave_repl_offset:2500\r\n" +
		"# Stats\r\n" +
		"total_commands_processed:5\r\n" +
		"instantaneous_ops_per_sec:11\r\n" +
		"keyspace_hits:6\r\n" +
		"keyspace_misses:1\r\n"

	tests := []struct {
		name            string
		client          redisClient
		fenced          bool
		fencedErr       error
		expectedRole    string
		expectedUp      float64
		expectedFenced  float64
		expectedCmds    float64
		expectedLag     float64
		expectLagMetric bool
	}{
		{
			name:           "master exports counters and gauges",
			client:         stubRedisClient{info: masterInfo},
			fenced:         true,
			expectedRole:   "master",
			expectedUp:     1,
			expectedFenced: 1,
			expectedCmds:   42,
		},
		{
			name:            "replica exports role and lag",
			client:          stubRedisClient{info: replicaInfo},
			expectedRole:    "slave",
			expectedUp:      1,
			expectedFenced:  0,
			expectedCmds:    5,
			expectedLag:     500,
			expectLagMetric: true,
		},
		{
			name:           "ping and info failures set up to zero",
			client:         stubRedisClient{infoErr: errors.New("info failed"), pingErr: errors.New("ping failed")},
			fencedErr:      errors.New("fenced check failed"),
			expectedRole:   "unknown",
			expectedUp:     0,
			expectedFenced: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			collector := NewRedisCollector(
				tt.client,
				"default",
				"redis-sample",
				"redis-sample-0",
				func(context.Context) (bool, error) {
					return tt.fenced, tt.fencedErr
				},
			)

			families := gatherMetricFamilies(t, collector)
			baseLabels := map[string]string{
				"namespace": "default",
				"cluster":   "redis-sample",
				"pod":       "redis-sample-0",
				"role":      tt.expectedRole,
			}

			upMetric := metricForLabels(t, families["redis_up"], baseLabels)
			require.NotNil(t, upMetric.GetGauge())
			assert.Equal(t, tt.expectedUp, upMetric.GetGauge().GetValue())

			fencedMetric := metricForLabels(t, families["redis_fenced"], baseLabels)
			require.NotNil(t, fencedMetric.GetGauge())
			assert.Equal(t, tt.expectedFenced, fencedMetric.GetGauge().GetValue())

			if tt.client.(stubRedisClient).infoErr == nil {
				cmdMetric := metricForLabels(t, families["redis_total_commands_processed"], baseLabels)
				require.NotNil(t, cmdMetric.GetCounter())
				assert.Equal(t, tt.expectedCmds, cmdMetric.GetCounter().GetValue())

				infoLabels := map[string]string{
					"namespace":     "default",
					"cluster":       "redis-sample",
					"pod":           "redis-sample-0",
					"role":          tt.expectedRole,
					"redis_version": "7.2.0",
					"os":            "Linux 6.8",
				}
				infoMetric := metricForLabels(t, families["redis_instance_info"], infoLabels)
				require.NotNil(t, infoMetric.GetGauge())
				assert.Equal(t, float64(1), infoMetric.GetGauge().GetValue())
			} else {
				_, hasInfo := families["redis_instance_info"]
				assert.False(t, hasInfo)
			}

			if tt.expectLagMetric {
				lagMetric := metricForLabels(t, families["redis_replication_lag_bytes"], baseLabels)
				require.NotNil(t, lagMetric.GetGauge())
				assert.Equal(t, tt.expectedLag, lagMetric.GetGauge().GetValue())
			}
		})
	}
}

type stubRedisClient struct {
	info    string
	infoErr error
	pingErr error
}

func (s stubRedisClient) Info(_ context.Context, _ ...string) *redis.StringCmd {
	return redis.NewStringResult(s.info, s.infoErr)
}

func (s stubRedisClient) Ping(_ context.Context) *redis.StatusCmd {
	return redis.NewStatusResult("PONG", s.pingErr)
}

func gatherMetricFamilies(t *testing.T, collector prometheus.Collector) map[string]*dto.MetricFamily {
	t.Helper()

	registry := prometheus.NewRegistry()
	require.NoError(t, registry.Register(collector))

	families, err := registry.Gather()
	require.NoError(t, err)

	byName := make(map[string]*dto.MetricFamily, len(families))
	for _, family := range families {
		byName[family.GetName()] = family
	}
	return byName
}

func metricForLabels(t *testing.T, family *dto.MetricFamily, labels map[string]string) *dto.Metric {
	t.Helper()
	require.NotNil(t, family)

	for _, metric := range family.GetMetric() {
		if hasExactLabelSet(metric, labels) {
			return metric
		}
	}
	t.Fatalf("metric %q with labels %v not found", family.GetName(), labels)
	return nil
}

func hasExactLabelSet(metric *dto.Metric, labels map[string]string) bool {
	if len(metric.GetLabel()) != len(labels) {
		return false
	}
	for _, label := range metric.GetLabel() {
		value, ok := labels[label.GetName()]
		if !ok || value != label.GetValue() {
			return false
		}
	}
	return true
}
