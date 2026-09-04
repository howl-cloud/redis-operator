package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	corev1 "k8s.io/api/core/v1"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// instanceManagerPort is the instance-manager HTTP port on every data pod.
// Tests point it at a local listener.
var instanceManagerPort = 8080

type clusterMeetPayload struct {
	IP   string `json:"ip"`
	Port int    `json:"port,omitempty"`
}

type clusterAddSlotsPayload struct {
	Start int32 `json:"start"`
	End   int32 `json:"end"`
}

type clusterReplicatePayload struct {
	NodeID string `json:"nodeID"`
}

func (r *ClusterReconciler) reconcileClusterBootstrap(
	ctx context.Context,
	cluster *redisv1.RedisCluster,
	statuses map[string]redisv1.InstanceStatus,
) (bool, error) {
	desired := int(cluster.Spec.DesiredDataInstances())
	if desired == 0 {
		return true, nil
	}

	pods, err := r.listDataPods(ctx, cluster)
	if err != nil {
		return false, fmt.Errorf("listing data pods: %w", err)
	}
	if len(pods) < desired {
		return true, nil
	}

	podsByName := make(map[string]corev1.Pod, len(pods))
	for i := range pods {
		podsByName[pods[i].Name] = pods[i]
	}

	httpClient := &http.Client{Timeout: statusPollTimeout}
	expectedNames := make([]string, 0, desired)
	for index := 0; index < desired; index++ {
		expectedNames = append(expectedNames, podNameForIndex(cluster.Name, index))
	}

	for _, podName := range expectedNames {
		pod, ok := podsByName[podName]
		if !ok || pod.Status.PodIP == "" {
			return true, nil
		}
		status, ok := statuses[podName]
		if !ok || !status.Connected || status.NodeID == "" {
			return true, nil
		}
	}

	coordinatorName := expectedNames[0]
	coordinatorIP := podsByName[coordinatorName].Status.PodIP
	performedAction := false

	for _, podName := range expectedNames[1:] {
		if err := postClusterJSON(ctx, httpClient, coordinatorIP, "/v1/cluster/meet", clusterMeetPayload{
			IP:   podsByName[podName].Status.PodIP,
			Port: 6379,
		}); err != nil {
			return false, fmt.Errorf("cluster meet %s via %s: %w", podName, coordinatorName, err)
		}
	}

	layout := planShardLayout(cluster, pods, statuses)
	shardCount := desiredShardCount(cluster)
	ranges := calculateClusterSlotRanges(shardCount)
	coverage := calculateSlotCoverage(statuses)
	for shardIndex := 0; shardIndex < shardCount; shardIndex++ {
		podName := layout.primaryOf[shardIndex]
		if podName == "" {
			return false, nil
		}
		for _, uncoveredRange := range calculateUncoveredSlotRanges(ranges[shardIndex], &coverage) {
			if err := postClusterJSON(
				ctx,
				httpClient,
				podsByName[podName].Status.PodIP,
				"/v1/cluster/addslots",
				clusterAddSlotsPayload{
					Start: uncoveredRange.Start,
					End:   uncoveredRange.End,
				},
			); err != nil {
				return false, fmt.Errorf(
					"assigning slots %d-%d on %s: %w",
					uncoveredRange.Start,
					uncoveredRange.End,
					podName,
					err,
				)
			}
			performedAction = true
			for slot := uncoveredRange.Start; slot <= uncoveredRange.End; slot++ {
				coverage[slot] = true
			}
		}
	}

	for _, podName := range expectedNames {
		if layout.isPrimary(podName) {
			continue
		}
		podStatus := statuses[podName]
		// A slot owner is never demoted here; reshard drains it first.
		if len(podStatus.SlotsServed) > 0 {
			continue
		}
		shardIndex := layout.shardOf[podName]
		primaryNodeID := statuses[layout.primaryOf[shardIndex]].NodeID
		if primaryNodeID == "" {
			return false, nil
		}
		// Older instance managers do not report primaryNodeID; leave those replicas alone.
		if podStatus.Role == "slave" && (podStatus.PrimaryNodeID == "" || podStatus.PrimaryNodeID == primaryNodeID) {
			continue
		}
		err := postClusterJSON(ctx, httpClient, podsByName[podName].Status.PodIP, "/v1/cluster/replicate", clusterReplicatePayload{
			NodeID: primaryNodeID,
		})
		var httpErr *clusterHTTPError
		if errors.As(err, &httpErr) && httpErr.status == http.StatusConflict {
			// The pod has not learned about its primary through gossip yet.
			return false, nil
		}
		if err != nil {
			return false, fmt.Errorf("replicating %s to shard %d: %w", podName, shardIndex, err)
		}
		performedAction = true
	}

	if performedAction {
		return false, nil
	}

	return cluster.Status.ClusterState == "ok" && cluster.Status.SlotsAssigned == 16384, nil
}

func calculateClusterSlotRanges(primaryCount int) []redisv1.SlotRange {
	if primaryCount <= 0 {
		return nil
	}
	base := 16384 / primaryCount
	remainder := 16384 % primaryCount
	ranges := make([]redisv1.SlotRange, 0, primaryCount)
	start := int32(0)
	for i := 0; i < primaryCount; i++ {
		size := base
		if i < remainder {
			size++
		}
		end := start + int32(size) - 1
		ranges = append(ranges, redisv1.SlotRange{Start: start, End: end})
		start = end + 1
	}
	return ranges
}

func calculateSlotCoverage(statuses map[string]redisv1.InstanceStatus) [16384]bool {
	var coverage [16384]bool
	for _, status := range statuses {
		for _, slotRange := range status.SlotsServed {
			start := slotRange.Start
			end := slotRange.End
			if start < 0 {
				start = 0
			}
			if end > 16383 {
				end = 16383
			}
			if end < start {
				continue
			}
			for slot := start; slot <= end; slot++ {
				coverage[slot] = true
			}
		}
	}
	return coverage
}

func calculateUncoveredSlotRanges(target redisv1.SlotRange, coverage *[16384]bool) []redisv1.SlotRange {
	start := target.Start
	end := target.End
	if start < 0 {
		start = 0
	}
	if end > 16383 {
		end = 16383
	}
	if end < start {
		return nil
	}

	var uncovered []redisv1.SlotRange
	slot := start
	for slot <= end {
		if coverage[slot] {
			slot++
			continue
		}
		rangeStart := slot
		for slot <= end && !coverage[slot] {
			slot++
		}
		uncovered = append(uncovered, redisv1.SlotRange{Start: rangeStart, End: slot - 1})
	}

	return uncovered
}

func postClusterJSON(ctx context.Context, httpClient *http.Client, podIP, endpoint string, payload any) error {
	if podIP == "" {
		return fmt.Errorf("pod IP is empty")
	}
	body, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshaling request payload: %w", err)
	}
	url := fmt.Sprintf("http://%s:%d%s", podIP, instanceManagerPort, endpoint)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(body))
	if err != nil {
		return fmt.Errorf("creating request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &clusterHTTPError{url: url, status: resp.StatusCode, body: strings.TrimSpace(string(body))}
	}
	return nil
}

type clusterHTTPError struct {
	url    string
	status int
	body   string
}

func (e *clusterHTTPError) Error() string {
	if e.body == "" {
		return fmt.Sprintf("POST %s returned status %d", e.url, e.status)
	}
	return fmt.Sprintf("POST %s returned status %d: %s", e.url, e.status, e.body)
}
