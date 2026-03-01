// Package replication provides helpers for configuring and querying Redis replication.
package replication

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"

	"github.com/redis/go-redis/v9"
)

// Info holds parsed replication information from Redis INFO replication.
type Info struct {
	Role                   string
	ConnectedReplicas      int
	MasterReplOffset       int64
	SlaveReplOffset        int64  // only set when Role == "slave"
	MasterLinkStatus       string // "up" or "down"
	MasterHost             string
	MasterPort             int
	MasterLastIOSecondsAgo int
}

// SlotRange describes an inclusive Redis hash slot range.
type SlotRange struct {
	Start int32
	End   int32
}

// ClusterInfo holds parsed values from CLUSTER INFO.
type ClusterInfo struct {
	State         string
	SlotsAssigned int32
	KnownNodes    int32
	CurrentEpoch  int64
}

// ClusterNode holds parsed values from CLUSTER NODES.
type ClusterNode struct {
	ID          string
	IP          string
	Port        int
	Flags       []string
	MasterID    string
	ConfigEpoch int64
	LinkState   string
	SlotRanges  []SlotRange
}

// GetInfo runs INFO replication and parses the response.
func GetInfo(ctx context.Context, client *redis.Client) (*Info, error) {
	result, err := client.Info(ctx, "replication").Result()
	if err != nil {
		return nil, fmt.Errorf("INFO replication: %w", err)
	}
	return parseInfo(result), nil
}

// SetReplicaOf issues REPLICAOF <primaryIP> <port>.
func SetReplicaOf(ctx context.Context, client *redis.Client, primaryIP string, port int) error {
	return client.SlaveOf(ctx, primaryIP, strconv.Itoa(port)).Err() //nolint:staticcheck // go-redis v9.18 exposes only SlaveOf; ReplicaOf is not yet available.
}

// Promote issues REPLICAOF NO ONE to promote this instance to primary.
func Promote(ctx context.Context, client *redis.Client) error {
	return client.SlaveOf(ctx, "NO", "ONE").Err() //nolint:staticcheck // go-redis v9.18 exposes only SlaveOf; ReplicaOf is not yet available.
}

// ClusterFailover issues CLUSTER FAILOVER [mode] (mode: FORCE or TAKEOVER).
func ClusterFailover(ctx context.Context, client *redis.Client, mode string) error {
	args := []interface{}{"CLUSTER", "FAILOVER"}
	if mode != "" {
		args = append(args, mode)
	}
	return client.Do(ctx, args...).Err()
}

// ClusterReplicate issues CLUSTER REPLICATE <nodeID>.
func ClusterReplicate(ctx context.Context, client *redis.Client, nodeID string) error {
	return client.Do(ctx, "CLUSTER", "REPLICATE", nodeID).Err()
}

// ClusterMeet issues CLUSTER MEET <ip> <port>.
func ClusterMeet(ctx context.Context, client *redis.Client, ip string, port int) error {
	return client.Do(ctx, "CLUSTER", "MEET", ip, strconv.Itoa(port)).Err()
}

// ClusterAddSlotsRange issues CLUSTER ADDSLOTSRANGE <start> <end>.
func ClusterAddSlotsRange(ctx context.Context, client *redis.Client, start, end int32) error {
	return client.Do(ctx, "CLUSTER", "ADDSLOTSRANGE", start, end).Err()
}

// ClusterDelSlotsRange issues CLUSTER DELSLOTSRANGE <start> <end>.
func ClusterDelSlotsRange(ctx context.Context, client *redis.Client, start, end int32) error {
	return client.Do(ctx, "CLUSTER", "DELSLOTSRANGE", start, end).Err()
}

// ClusterSetSlotMigrating marks a slot as migrating to the target node.
func ClusterSetSlotMigrating(ctx context.Context, client *redis.Client, slot int32, targetNodeID string) error {
	return client.Do(ctx, "CLUSTER", "SETSLOT", slot, "MIGRATING", targetNodeID).Err()
}

// ClusterSetSlotImporting marks a slot as importing from the source node.
func ClusterSetSlotImporting(ctx context.Context, client *redis.Client, slot int32, sourceNodeID string) error {
	return client.Do(ctx, "CLUSTER", "SETSLOT", slot, "IMPORTING", sourceNodeID).Err()
}

// ClusterSetSlotNode assigns a slot to the given node ID.
func ClusterSetSlotNode(ctx context.Context, client *redis.Client, slot int32, nodeID string) error {
	return client.Do(ctx, "CLUSTER", "SETSLOT", slot, "NODE", nodeID).Err()
}

// ClusterGetKeysInSlot returns up to count keys from a slot.
func ClusterGetKeysInSlot(ctx context.Context, client *redis.Client, slot, count int32) ([]string, error) {
	result, err := client.Do(ctx, "CLUSTER", "GETKEYSINSLOT", slot, count).Result()
	if err != nil {
		return nil, fmt.Errorf("CLUSTER GETKEYSINSLOT %d %d: %w", slot, count, err)
	}
	values, ok := result.([]interface{})
	if !ok {
		return nil, fmt.Errorf("CLUSTER GETKEYSINSLOT returned unexpected type %T", result)
	}
	keys := make([]string, 0, len(values))
	for _, value := range values {
		key, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("CLUSTER GETKEYSINSLOT returned non-string key type %T", value)
		}
		keys = append(keys, key)
	}
	return keys, nil
}

// MigrateKeys migrates keys to the target endpoint using MIGRATE ... KEYS.
func MigrateKeys(ctx context.Context, client *redis.Client, targetIP string, targetPort, timeoutMillis int, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	args := make([]interface{}, 0, 7+len(keys))
	args = append(args,
		"MIGRATE",
		targetIP,
		strconv.Itoa(targetPort),
		"",
		"0",
		strconv.Itoa(timeoutMillis),
		"KEYS",
	)
	for _, key := range keys {
		args = append(args, key)
	}
	if err := client.Do(ctx, args...).Err(); err != nil {
		return fmt.Errorf("MIGRATE %s:%d keys=%d: %w", targetIP, targetPort, len(keys), err)
	}
	return nil
}

// GetClusterInfo runs CLUSTER INFO and parses the response.
func GetClusterInfo(ctx context.Context, client *redis.Client) (*ClusterInfo, error) {
	result, err := client.ClusterInfo(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("CLUSTER INFO: %w", err)
	}
	return parseClusterInfo(result), nil
}

// GetClusterNodes runs CLUSTER NODES and parses the response.
func GetClusterNodes(ctx context.Context, client *redis.Client) ([]ClusterNode, error) {
	result, err := client.ClusterNodes(ctx).Result()
	if err != nil {
		return nil, fmt.Errorf("CLUSTER NODES: %w", err)
	}
	return parseClusterNodes(result), nil
}

// ReplicationOffset returns the current replication offset.
func ReplicationOffset(ctx context.Context, client *redis.Client) (int64, error) {
	info, err := GetInfo(ctx, client)
	if err != nil {
		return 0, err
	}
	if info.Role == "master" {
		return info.MasterReplOffset, nil
	}
	return info.SlaveReplOffset, nil
}

// IsConnectedToPrimary returns true if this replica has an active link to the primary.
func IsConnectedToPrimary(ctx context.Context, client *redis.Client) (bool, error) {
	info, err := GetInfo(ctx, client)
	if err != nil {
		return false, err
	}
	return info.MasterLinkStatus == "up", nil
}

func parseInfo(raw string) *Info {
	info := &Info{}
	for _, line := range strings.Split(raw, "\r\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "role":
			info.Role = val
		case "connected_slaves":
			info.ConnectedReplicas, _ = strconv.Atoi(val)
		case "master_repl_offset":
			info.MasterReplOffset, _ = strconv.ParseInt(val, 10, 64)
		case "slave_repl_offset":
			info.SlaveReplOffset, _ = strconv.ParseInt(val, 10, 64)
		case "master_link_status":
			info.MasterLinkStatus = val
		case "master_host":
			info.MasterHost = val
		case "master_port":
			info.MasterPort, _ = strconv.Atoi(val)
		case "master_last_io_seconds_ago":
			info.MasterLastIOSecondsAgo, _ = strconv.Atoi(val)
		}
	}
	return info
}

func parseClusterInfo(raw string) *ClusterInfo {
	info := &ClusterInfo{}
	for _, line := range strings.Split(raw, "\r\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key, val := parts[0], parts[1]
		switch key {
		case "cluster_state":
			info.State = val
		case "cluster_slots_assigned":
			n, _ := strconv.ParseInt(val, 10, 32)
			info.SlotsAssigned = int32(n)
		case "cluster_known_nodes":
			n, _ := strconv.ParseInt(val, 10, 32)
			info.KnownNodes = int32(n)
		case "cluster_current_epoch":
			info.CurrentEpoch, _ = strconv.ParseInt(val, 10, 64)
		}
	}
	return info
}

func parseClusterNodes(raw string) []ClusterNode {
	var nodes []ClusterNode
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 8 {
			continue
		}
		ip, port := parseClusterNodeAddress(fields[1])
		epoch, _ := strconv.ParseInt(fields[6], 10, 64)

		node := ClusterNode{
			ID:          fields[0],
			IP:          ip,
			Port:        port,
			Flags:       strings.Split(fields[2], ","),
			MasterID:    fields[3],
			ConfigEpoch: epoch,
			LinkState:   fields[7],
		}
		node.SlotRanges = parseSlotRanges(fields[8:])
		nodes = append(nodes, node)
	}
	return nodes
}

func parseClusterNodeAddress(addr string) (string, int) {
	mainAddr := strings.Split(addr, "@")[0]
	host, portStr, err := net.SplitHostPort(mainAddr)
	if err != nil {
		return "", 0
	}
	port, _ := strconv.Atoi(portStr)
	return host, port
}

func parseSlotRanges(tokens []string) []SlotRange {
	ranges := make([]SlotRange, 0, len(tokens))
	for _, token := range tokens {
		if strings.HasPrefix(token, "[") {
			continue
		}
		if !strings.Contains(token, "-") {
			slot, err := strconv.ParseInt(token, 10, 32)
			if err != nil {
				continue
			}
			ranges = append(ranges, SlotRange{Start: int32(slot), End: int32(slot)})
			continue
		}
		parts := strings.SplitN(token, "-", 2)
		if len(parts) != 2 {
			continue
		}
		start, err := strconv.ParseInt(parts[0], 10, 32)
		if err != nil {
			continue
		}
		end, err := strconv.ParseInt(parts[1], 10, 32)
		if err != nil {
			continue
		}
		ranges = append(ranges, SlotRange{Start: int32(start), End: int32(end)})
	}
	return ranges
}
