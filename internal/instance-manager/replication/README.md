# internal/instance-manager/replication

Helpers for configuring and querying Redis replication via the Redis protocol.

## Functions

| Function | Description |
|----------|-------------|
| `GetInfo(ctx)` | Runs `INFO replication` and parses the response into a struct |
| `SetReplicaOf(ctx, primaryIP, port)` | Issues `REPLICAOF <ip> <port>` |
| `Promote(ctx)` | Issues `REPLICAOF NO ONE` |
| `ReplicationOffset(ctx)` | Returns the current replication offset (for lag-based failover candidate selection) |
| `IsConnectedToPrimary(ctx)` | Returns true if this replica has an active link to the primary |

## Replication Info Struct

```go
type Info struct {
    Role             string // "master" or "slave"
    ConnectedReplicas int
    MasterReplOffset  int64
    SlaveReplOffset   int64  // only set when role == "slave"
    MasterLinkStatus  string // "up" or "down"
    MasterLastIOSecondsAgo int
}
```
