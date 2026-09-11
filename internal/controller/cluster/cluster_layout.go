package cluster

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// shardLayout maps every data pod of a cluster-mode RedisCluster to a shard.
//
// Membership is decided in this order: a pod that owns slots is the primary of
// the shard whose slot range it serves most of; a replica belongs to the shard
// of the primary it follows; a pod whose topology is not observable keeps its
// redis.io/shard label; every other pod is empty and is placed to fill missing
// primaries first, then to balance replicas. Pod ordinals never decide
// membership by themselves, so changing spec.replicasPerShard cannot turn an
// existing primary into a replica.
//
// A slot owner whose ordinal is beyond the desired pod count is about to be
// deleted by a scale-down. It stays the shard's owner (ownerOf) but a surviving
// pod is chosen as primaryOf, and reshard hands the shard over before the
// owner goes away.
type shardLayout struct {
	shardOf      map[string]int
	primaryOf    map[int]string
	ownerOf      map[int]string
	members      map[int][]string
	replicaMoves map[string]int
}

// handingOver reports whether the shard's slots still sit on a pod other than
// its chosen primary.
func (l shardLayout) handingOver(shard int) bool {
	owner, ok := l.ownerOf[shard]
	return ok && owner != l.primaryOf[shard]
}

func (l shardLayout) isPrimary(podName string) bool {
	shard, ok := l.shardOf[podName]
	return ok && l.primaryOf[shard] == podName
}

func (l shardLayout) labels(podName string) map[string]string {
	shard, ok := l.shardOf[podName]
	if !ok {
		return nil
	}
	role := redisv1.LabelRoleReplica
	if l.primaryOf[shard] == podName {
		role = redisv1.LabelRolePrimary
	}
	return map[string]string{
		redisv1.LabelShard:     shardName(shard),
		redisv1.LabelShardRole: role,
	}
}

// desiredShardCount mirrors the clamp in RedisClusterSpec.DesiredDataInstances.
func desiredShardCount(cluster *redisv1.RedisCluster) int {
	if cluster.Spec.Shards < 3 {
		return 3
	}
	return int(cluster.Spec.Shards)
}

func shardName(index int) string {
	return fmt.Sprintf("s%d", index)
}

func parseShardLabel(pod *corev1.Pod) (int, bool) {
	value, ok := pod.Labels[redisv1.LabelShard]
	if !ok || !strings.HasPrefix(value, "s") {
		return 0, false
	}
	index, err := strconv.Atoi(value[1:])
	if err != nil || index < 0 {
		return 0, false
	}
	return index, true
}

func planShardLayout(
	cluster *redisv1.RedisCluster,
	pods []corev1.Pod,
	statuses map[string]redisv1.InstanceStatus,
) shardLayout {
	layout := shardLayout{
		shardOf:   make(map[string]int),
		primaryOf: make(map[int]string),
		ownerOf:   make(map[int]string),
		members:   make(map[int][]string),
	}
	if cluster == nil {
		return layout
	}

	shardCount := desiredShardCount(cluster)
	desired := int(cluster.Spec.DesiredDataInstances())
	doomed := func(name string) bool { return podIndex(cluster.Name, name) >= desired }

	seen := make(map[string]bool)
	var names []string
	for index := 0; index < desired; index++ {
		name := podNameForIndex(cluster.Name, index)
		seen[name] = true
		names = append(names, name)
	}
	labelOf := make(map[string]int)
	primaryLabel := make(map[string]bool)
	for i := range pods {
		pod := &pods[i]
		if !seen[pod.Name] {
			seen[pod.Name] = true
			names = append(names, pod.Name)
		}
		if index, ok := parseShardLabel(pod); ok {
			labelOf[pod.Name] = index
		}
		primaryLabel[pod.Name] = pod.Labels[redisv1.LabelShardRole] == redisv1.LabelRolePrimary
	}
	sort.Slice(names, func(i, j int) bool {
		return podIndex(cluster.Name, names[i]) < podIndex(cluster.Name, names[j])
	})

	used := make(map[int]bool)
	assign := func(name string, shard int) {
		layout.shardOf[name] = shard
		layout.members[shard] = append(layout.members[shard], name)
		used[shard] = true
	}
	lowestUnused := func() int {
		for shard := 0; ; shard++ {
			if !used[shard] {
				return shard
			}
		}
	}

	// Slot owners define shards. Each owner takes the shard whose canonical
	// slot range it already serves most of, so a reshard moves as little data
	// as possible and labels can never cause one. Owners left over (more owners
	// than shards, or no overlap) take the lowest free index and get drained.
	var owners []string
	for _, name := range names {
		if len(statuses[name].SlotsServed) > 0 {
			owners = append(owners, name)
		}
	}
	claim := func(name string, shard int) {
		assign(name, shard)
		layout.ownerOf[shard] = name
		if !doomed(name) {
			layout.primaryOf[shard] = name
		}
	}
	for _, match := range rankOwnersByOverlap(owners, statuses, calculateClusterSlotRanges(shardCount)) {
		if _, done := layout.shardOf[match.pod]; done || used[match.shard] {
			continue
		}
		claim(match.pod, match.shard)
	}
	for _, name := range owners {
		if _, done := layout.shardOf[name]; done {
			continue
		}
		claim(name, lowestUnused())
	}

	podByNodeID := make(map[string]string, len(statuses))
	for name, status := range statuses {
		if status.NodeID != "" {
			podByNodeID[status.NodeID] = name
		}
	}

	var free []string
	for _, name := range names {
		if _, done := layout.shardOf[name]; done {
			continue
		}
		status, polled := statuses[name]
		if status.Role == "slave" && status.PrimaryNodeID != "" {
			if shard, ok := layout.shardOf[podByNodeID[status.PrimaryNodeID]]; ok {
				assign(name, shard)
				// A surviving replica is the best heir of a doomed owner.
				if layout.primaryOf[shard] == "" && !doomed(name) {
					layout.primaryOf[shard] = name
				}
				continue
			}
		}

		// Nothing observable: trust the label until the pod reports in.
		observable := polled && status.Connected && (status.Role != "slave" || status.PrimaryNodeID != "")
		if shard, ok := labelOf[name]; ok && !observable && shard < shardCount {
			assign(name, shard)
			if primaryLabel[name] && layout.primaryOf[shard] == "" && !doomed(name) {
				layout.primaryOf[shard] = name
			}
			continue
		}
		free = append(free, name)
	}

	var survivors []string
	for _, name := range free {
		if !doomed(name) {
			survivors = append(survivors, name)
		}
	}
	for shard := 0; shard < shardCount && len(survivors) > 0; shard++ {
		if layout.primaryOf[shard] != "" {
			continue
		}
		assign(survivors[0], shard)
		layout.primaryOf[shard] = survivors[0]
		survivors = survivors[1:]
	}
	free = append(survivors, filter(free, doomed)...)

	// A doomed owner with no surviving replica and no free survivor borrows a
	// surviving replica from the shard that can spare one most easily.
	for shard := 0; shard < shardCount; shard++ {
		if layout.primaryOf[shard] != "" || layout.ownerOf[shard] == "" {
			continue
		}
		heir := layout.spareSurvivor(doomed)
		if heir == "" {
			break
		}
		layout.move(heir, shard)
		layout.primaryOf[shard] = heir
	}

	for _, name := range free {
		target := 0
		for shard := 1; shard < shardCount; shard++ {
			if len(layout.members[shard]) < len(layout.members[target]) {
				target = shard
			}
		}
		assign(name, target)
	}

	for shard := range layout.members {
		members := layout.members[shard]
		sort.Slice(members, func(i, j int) bool {
			return podIndex(cluster.Name, members[i]) < podIndex(cluster.Name, members[j])
		})
	}
	layout.planReplicaMoves(cluster, statuses, doomed)
	return layout
}

// spareSurvivor returns the lowest-ordinal surviving non-primary from the shard
// with the most members, or "" when none exists.
func (l shardLayout) spareSurvivor(doomed func(string) bool) string {
	best, bestSize := "", 1
	for shard, members := range l.members {
		if len(members) <= bestSize {
			continue
		}
		for _, name := range members {
			if name != l.primaryOf[shard] && !doomed(name) {
				best, bestSize = name, len(members)
				break
			}
		}
	}
	return best
}

func (l shardLayout) move(name string, shard int) {
	from := l.shardOf[name]
	members := l.members[from]
	for i, member := range members {
		if member == name {
			l.members[from] = append(members[:i], members[i+1:]...)
			break
		}
	}
	l.shardOf[name] = shard
	l.members[shard] = append(l.members[shard], name)
}

func filter(names []string, keep func(string) bool) []string {
	var out []string
	for _, name := range names {
		if keep(name) {
			out = append(out, name)
		}
	}
	return out
}

type ownerMatch struct {
	pod     string
	shard   int
	overlap int32
}

// rankOwnersByOverlap lists every (owner, shard) pair with a non-zero slot
// overlap, best match first. Ties go to the lower shard, then the lower pod.
func rankOwnersByOverlap(owners []string, statuses map[string]redisv1.InstanceStatus, ranges []redisv1.SlotRange) []ownerMatch {
	var matches []ownerMatch
	for _, name := range owners {
		for shard, target := range ranges {
			var overlap int32
			for _, served := range statuses[name].SlotsServed {
				start := max(served.Start, target.Start)
				end := min(served.End, target.End)
				if end >= start {
					overlap += end - start + 1
				}
			}
			if overlap > 0 {
				matches = append(matches, ownerMatch{pod: name, shard: shard, overlap: overlap})
			}
		}
	}
	sort.SliceStable(matches, func(a, b int) bool {
		if matches[a].overlap != matches[b].overlap {
			return matches[a].overlap > matches[b].overlap
		}
		return matches[a].shard < matches[b].shard
	})
	return matches
}

// planReplicaMoves balances surviving replicas without changing observed membership.
func (l *shardLayout) planReplicaMoves(cluster *redisv1.RedisCluster, statuses map[string]redisv1.InstanceStatus, doomed func(string) bool) {
	shardCount := desiredShardCount(cluster)
	counts := make([]int, shardCount)
	for shard := 0; shard < shardCount; shard++ {
		if l.primaryOf[shard] == "" || l.handingOver(shard) {
			return
		}
		for _, name := range l.members[shard] {
			if !doomed(name) && !l.isPrimary(name) {
				counts[shard]++
			}
		}
	}
	desired := int(cluster.Spec.ReplicasPerShard)
	for target := 0; target < shardCount; target++ {
		for source := 0; source < shardCount && counts[target] < desired; source++ {
			members := l.members[source]
			for i := len(members) - 1; i >= 0 && counts[source] > desired && counts[target] < desired; i-- {
				name := members[i]
				status := statuses[name]
				if _, moving := l.replicaMoves[name]; moving {
					continue
				}
				if doomed(name) || l.isPrimary(name) || !status.Connected || status.Role != "slave" || status.PrimaryNodeID == "" || len(status.SlotsServed) != 0 {
					continue
				}
				if l.replicaMoves == nil {
					l.replicaMoves = make(map[string]int)
				}
				l.replicaMoves[name] = target
				counts[source]--
				counts[target]++
			}
		}
	}
}
