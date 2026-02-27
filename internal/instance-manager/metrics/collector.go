// Package metrics provides Prometheus collectors for Redis instance-manager pods.
package metrics

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"
)

const (
	metricNamespace = "redis"
	unknownRole     = "unknown"
)

var metricLabels = []string{"namespace", "cluster", "pod", "role"}

type redisClient interface {
	Info(ctx context.Context, section ...string) *redis.StringCmd
	Ping(ctx context.Context) *redis.StatusCmd
}

// FencedCheckFunc returns whether the current pod is fenced.
type FencedCheckFunc func(ctx context.Context) (bool, error)

// RedisCollector collects Redis INFO-based metrics with Kubernetes identity labels.
type RedisCollector struct {
	client      redisClient
	namespace   string
	cluster     string
	pod         string
	fencedCheck FencedCheckFunc

	upDesc                      *prometheus.Desc
	instanceInfoDesc            *prometheus.Desc
	connectedClientsDesc        *prometheus.Desc
	blockedClientsDesc          *prometheus.Desc
	rejectedConnectionsDesc     *prometheus.Desc
	usedMemoryDesc              *prometheus.Desc
	usedMemoryRSSDesc           *prometheus.Desc
	usedMemoryPeakDesc          *prometheus.Desc
	maxMemoryDesc               *prometheus.Desc
	memFragmentationRatioDesc   *prometheus.Desc
	evictedKeysDesc             *prometheus.Desc
	totalCommandsProcessedDesc  *prometheus.Desc
	commandCallsDesc            *prometheus.Desc
	instantaneousOpsPerSecDesc  *prometheus.Desc
	keyspaceHitsDesc            *prometheus.Desc
	keyspaceMissesDesc          *prometheus.Desc
	connectedReplicasDesc       *prometheus.Desc
	replicationOffsetDesc       *prometheus.Desc
	replicaReplOffsetDesc       *prometheus.Desc
	replicationLagBytesDesc     *prometheus.Desc
	masterLinkUpDesc            *prometheus.Desc
	rdbLastSaveTimestampDesc    *prometheus.Desc
	rdbLastBgsaveDurationDesc   *prometheus.Desc
	aofLastRewriteDurationDesc  *prometheus.Desc
	rdbChangesSinceLastSaveDesc *prometheus.Desc
	loadingDesc                 *prometheus.Desc
	fencedDesc                  *prometheus.Desc
	uptimeDesc                  *prometheus.Desc
}

// NewRedisCollector returns a Prometheus collector for Redis instance metrics.
func NewRedisCollector(
	client redisClient,
	namespace, cluster, pod string,
	fencedCheck FencedCheckFunc,
) *RedisCollector {
	return &RedisCollector{
		client:      client,
		namespace:   namespace,
		cluster:     cluster,
		pod:         pod,
		fencedCheck: fencedCheck,
		upDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "up"),
			"Whether Redis is up (1) or down (0), based on PING.",
			metricLabels,
			nil,
		),
		instanceInfoDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "instance_info"),
			"Static Redis instance information.",
			append(metricLabels, "redis_version", "os"),
			nil,
		),
		connectedClientsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "connected_clients"),
			"Number of client connections.",
			metricLabels,
			nil,
		),
		blockedClientsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "blocked_clients"),
			"Number of clients pending on a blocking call.",
			metricLabels,
			nil,
		),
		rejectedConnectionsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "rejected_connections_total"),
			"Total number of connections rejected due to maxclients or other limits.",
			metricLabels,
			nil,
		),
		usedMemoryDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "used_memory_bytes"),
			"Total memory used by Redis in bytes.",
			metricLabels,
			nil,
		),
		usedMemoryRSSDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "used_memory_rss_bytes"),
			"Resident set size memory used by Redis in bytes.",
			metricLabels,
			nil,
		),
		usedMemoryPeakDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "used_memory_peak_bytes"),
			"Peak memory consumed by Redis in bytes.",
			metricLabels,
			nil,
		),
		maxMemoryDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "maxmemory_bytes"),
			"Configured maxmemory in bytes.",
			metricLabels,
			nil,
		),
		memFragmentationRatioDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "mem_fragmentation_ratio"),
			"Memory fragmentation ratio.",
			metricLabels,
			nil,
		),
		evictedKeysDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "evicted_keys_total"),
			"Total number of evicted keys.",
			metricLabels,
			nil,
		),
		totalCommandsProcessedDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "total_commands_processed"),
			"Total number of commands processed by the server.",
			metricLabels,
			nil,
		),
		commandCallsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "command_calls_total"),
			"Total number of calls for a specific Redis command.",
			append(metricLabels, "command"),
			nil,
		),
		instantaneousOpsPerSecDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "instantaneous_ops_per_sec"),
			"Number of operations per second over the last sampling interval.",
			metricLabels,
			nil,
		),
		keyspaceHitsDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "keyspace_hits_total"),
			"Number of successful lookup of keys.",
			metricLabels,
			nil,
		),
		keyspaceMissesDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "keyspace_misses_total"),
			"Number of failed lookup of keys.",
			metricLabels,
			nil,
		),
		connectedReplicasDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "connected_replicas"),
			"Number of connected replicas.",
			metricLabels,
			nil,
		),
		replicationOffsetDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "replication_offset"),
			"Replication offset for the primary.",
			metricLabels,
			nil,
		),
		replicaReplOffsetDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "replica_repl_offset"),
			"Replication offset for the replica.",
			metricLabels,
			nil,
		),
		replicationLagBytesDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "replication_lag_bytes"),
			"Replication lag in bytes between master and replica offsets.",
			metricLabels,
			nil,
		),
		masterLinkUpDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "master_link_up"),
			"Whether the replica link to master is up (1) or down (0).",
			metricLabels,
			nil,
		),
		rdbLastSaveTimestampDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "rdb_last_save_timestamp"),
			"Last successful RDB save time (unix timestamp).",
			metricLabels,
			nil,
		),
		rdbLastBgsaveDurationDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "rdb_last_bgsave_duration_seconds"),
			"Duration of the last RDB background save in seconds.",
			metricLabels,
			nil,
		),
		aofLastRewriteDurationDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "aof_last_rewrite_duration_seconds"),
			"Duration of the last AOF rewrite in seconds.",
			metricLabels,
			nil,
		),
		rdbChangesSinceLastSaveDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "rdb_changes_since_last_save"),
			"Number of changes since the last RDB save.",
			metricLabels,
			nil,
		),
		loadingDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "loading"),
			"Whether Redis is loading data from disk (1) or not (0).",
			metricLabels,
			nil,
		),
		fencedDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "fenced"),
			"Whether this Redis instance is fenced (1) or not (0).",
			metricLabels,
			nil,
		),
		uptimeDesc: prometheus.NewDesc(
			prometheus.BuildFQName(metricNamespace, "", "uptime_seconds"),
			"Number of seconds since Redis server start.",
			metricLabels,
			nil,
		),
	}
}

// Describe sends all metric descriptors.
func (c *RedisCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.upDesc
	ch <- c.instanceInfoDesc
	ch <- c.connectedClientsDesc
	ch <- c.blockedClientsDesc
	ch <- c.rejectedConnectionsDesc
	ch <- c.usedMemoryDesc
	ch <- c.usedMemoryRSSDesc
	ch <- c.usedMemoryPeakDesc
	ch <- c.maxMemoryDesc
	ch <- c.memFragmentationRatioDesc
	ch <- c.evictedKeysDesc
	ch <- c.totalCommandsProcessedDesc
	ch <- c.commandCallsDesc
	ch <- c.instantaneousOpsPerSecDesc
	ch <- c.keyspaceHitsDesc
	ch <- c.keyspaceMissesDesc
	ch <- c.connectedReplicasDesc
	ch <- c.replicationOffsetDesc
	ch <- c.replicaReplOffsetDesc
	ch <- c.replicationLagBytesDesc
	ch <- c.masterLinkUpDesc
	ch <- c.rdbLastSaveTimestampDesc
	ch <- c.rdbLastBgsaveDurationDesc
	ch <- c.aofLastRewriteDurationDesc
	ch <- c.rdbChangesSinceLastSaveDesc
	ch <- c.loadingDesc
	ch <- c.fencedDesc
	ch <- c.uptimeDesc
}

// Collect gathers metrics from Redis INFO and emits them.
func (c *RedisCollector) Collect(ch chan<- prometheus.Metric) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	up := 0.0
	if c.client.Ping(ctx).Err() == nil {
		up = 1
	}

	infoRaw, infoErr := c.client.Info(ctx, "all").Result()
	info := map[string]string{}
	role := unknownRole
	if infoErr == nil {
		info = parseInfo(infoRaw)
		role = normalizeRole(info["role"])
	}

	labels := c.baseLabelValues(role)
	ch <- prometheus.MustNewConstMetric(c.upDesc, prometheus.GaugeValue, up, labels...)

	fenced := 0.0
	if c.fencedCheck != nil {
		if isFenced, err := c.fencedCheck(ctx); err == nil && isFenced {
			fenced = 1
		}
	}
	ch <- prometheus.MustNewConstMetric(c.fencedDesc, prometheus.GaugeValue, fenced, labels...)

	if infoErr != nil {
		return
	}

	version := info["redis_version"]
	osName := info["os"]
	ch <- prometheus.MustNewConstMetric(
		c.instanceInfoDesc,
		prometheus.GaugeValue,
		1,
		append(labels, version, osName)...,
	)

	c.emitFromKey(ch, c.connectedClientsDesc, prometheus.GaugeValue, info, "connected_clients", labels)
	c.emitFromKey(ch, c.blockedClientsDesc, prometheus.GaugeValue, info, "blocked_clients", labels)
	c.emitFromKey(ch, c.rejectedConnectionsDesc, prometheus.CounterValue, info, "rejected_connections", labels)
	c.emitFromKey(ch, c.usedMemoryDesc, prometheus.GaugeValue, info, "used_memory", labels)
	c.emitFromKey(ch, c.usedMemoryRSSDesc, prometheus.GaugeValue, info, "used_memory_rss", labels)
	c.emitFromKey(ch, c.usedMemoryPeakDesc, prometheus.GaugeValue, info, "used_memory_peak", labels)
	c.emitFromKey(ch, c.maxMemoryDesc, prometheus.GaugeValue, info, "maxmemory", labels)
	c.emitFromKey(ch, c.memFragmentationRatioDesc, prometheus.GaugeValue, info, "mem_fragmentation_ratio", labels)
	c.emitFromKey(ch, c.evictedKeysDesc, prometheus.CounterValue, info, "evicted_keys", labels)
	c.emitFromKey(ch, c.totalCommandsProcessedDesc, prometheus.CounterValue, info, "total_commands_processed", labels)
	c.emitCommandCalls(ch, info, labels)
	c.emitFromKey(ch, c.instantaneousOpsPerSecDesc, prometheus.GaugeValue, info, "instantaneous_ops_per_sec", labels)
	c.emitFromKey(ch, c.keyspaceHitsDesc, prometheus.CounterValue, info, "keyspace_hits", labels)
	c.emitFromKey(ch, c.keyspaceMissesDesc, prometheus.CounterValue, info, "keyspace_misses", labels)
	c.emitFromKey(ch, c.connectedReplicasDesc, prometheus.GaugeValue, info, "connected_slaves", labels)
	c.emitFromKey(ch, c.replicationOffsetDesc, prometheus.GaugeValue, info, "master_repl_offset", labels)
	c.emitFromKey(ch, c.replicaReplOffsetDesc, prometheus.GaugeValue, info, "slave_repl_offset", labels)
	c.emitFromKey(ch, c.rdbLastSaveTimestampDesc, prometheus.GaugeValue, info, "rdb_last_save_time", labels)
	c.emitFromKey(ch, c.rdbLastBgsaveDurationDesc, prometheus.GaugeValue, info, "rdb_last_bgsave_time_sec", labels)
	c.emitFromKey(ch, c.aofLastRewriteDurationDesc, prometheus.GaugeValue, info, "aof_last_rewrite_time_sec", labels)
	c.emitFromKey(ch, c.rdbChangesSinceLastSaveDesc, prometheus.GaugeValue, info, "rdb_changes_since_last_save", labels)
	c.emitFromKey(ch, c.loadingDesc, prometheus.GaugeValue, info, "loading", labels)
	c.emitFromKey(ch, c.uptimeDesc, prometheus.GaugeValue, info, "uptime_in_seconds", labels)

	masterLinkUp := 0.0
	if strings.EqualFold(info["master_link_status"], "up") {
		masterLinkUp = 1
	}
	ch <- prometheus.MustNewConstMetric(c.masterLinkUpDesc, prometheus.GaugeValue, masterLinkUp, labels...)

	masterOffset, okMaster := parseFloatKey(info, "master_repl_offset")
	slaveOffset, okSlave := parseFloatKey(info, "slave_repl_offset")
	if okMaster && okSlave {
		lag := masterOffset - slaveOffset
		if lag < 0 {
			lag = 0
		}
		ch <- prometheus.MustNewConstMetric(c.replicationLagBytesDesc, prometheus.GaugeValue, lag, labels...)
	}
}

func (c *RedisCollector) baseLabelValues(role string) []string {
	return []string{
		c.namespace,
		c.cluster,
		c.pod,
		normalizeRole(role),
	}
}

func (c *RedisCollector) emitFromKey(
	ch chan<- prometheus.Metric,
	desc *prometheus.Desc,
	valueType prometheus.ValueType,
	info map[string]string,
	key string,
	labels []string,
) {
	value, ok := parseFloatKey(info, key)
	if !ok {
		return
	}
	ch <- prometheus.MustNewConstMetric(desc, valueType, value, labels...)
}

func (c *RedisCollector) emitCommandCalls(
	ch chan<- prometheus.Metric,
	info map[string]string,
	labels []string,
) {
	commands := []string{"get", "set", "del"}
	for _, command := range commands {
		raw, ok := info["cmdstat_"+command]
		if !ok {
			continue
		}
		calls, ok := parseCommandCalls(raw)
		if !ok {
			continue
		}
		ch <- prometheus.MustNewConstMetric(
			c.commandCallsDesc,
			prometheus.CounterValue,
			calls,
			append(labels, command)...,
		)
	}
}

func parseInfo(infoRaw string) map[string]string {
	values := make(map[string]string)
	for _, line := range strings.Split(infoRaw, "\r\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		values[parts[0]] = parts[1]
	}
	return values
}

func parseFloatKey(values map[string]string, key string) (float64, bool) {
	raw, ok := values[key]
	if !ok || raw == "" {
		return 0, false
	}
	parsed, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return 0, false
	}
	return parsed, true
}

func parseCommandCalls(raw string) (float64, bool) {
	if strings.TrimSpace(raw) == "" {
		return 0, false
	}

	entries := strings.Split(raw, ",")
	for _, entry := range entries {
		parts := strings.SplitN(entry, "=", 2)
		if len(parts) != 2 || parts[0] != "calls" {
			continue
		}
		calls, err := strconv.ParseFloat(parts[1], 64)
		if err != nil {
			return 0, false
		}
		return calls, true
	}

	return 0, false
}

func normalizeRole(role string) string {
	if strings.TrimSpace(role) == "" {
		return unknownRole
	}
	return role
}
