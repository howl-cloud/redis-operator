// Package replication provides helpers for configuring and querying Redis replication.
package replication

import (
	"context"
	"fmt"
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
	MasterLastIOSecondsAgo int
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
		case "master_last_io_seconds_ago":
			info.MasterLastIOSecondsAgo, _ = strconv.Atoi(val)
		}
	}
	return info
}
