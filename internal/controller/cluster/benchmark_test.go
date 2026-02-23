package cluster

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const benchmarkInstancesPerCluster int32 = 3

var benchmarkPodStatusResponse = []byte(`{"role":"master","replicationOffset":12345,"connectedReplicas":2,"connected":true}`)

type benchmarkStatusRoundTripper struct {
	responseBody []byte
	requestCount atomic.Int64
}

func (rt *benchmarkStatusRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.requestCount.Add(1)
	return &http.Response{
		StatusCode: http.StatusOK,
		Status:     "200 OK",
		Header:     http.Header{"Content-Type": []string{"application/json"}},
		Body:       io.NopCloser(bytes.NewReader(rt.responseBody)),
		Request:    req,
	}, nil
}

func (rt *benchmarkStatusRoundTripper) Reset() {
	rt.requestCount.Store(0)
}

func (rt *benchmarkStatusRoundTripper) Count() int64 {
	return rt.requestCount.Load()
}

type benchmarkRequestCountingClient struct {
	client.Client
	requestCount atomic.Int64
}

func (c *benchmarkRequestCountingClient) Get(ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
	c.requestCount.Add(1)
	return c.Client.Get(ctx, key, obj, opts...)
}

func (c *benchmarkRequestCountingClient) List(ctx context.Context, list client.ObjectList, opts ...client.ListOption) error {
	c.requestCount.Add(1)
	return c.Client.List(ctx, list, opts...)
}

func (c *benchmarkRequestCountingClient) Create(ctx context.Context, obj client.Object, opts ...client.CreateOption) error {
	c.requestCount.Add(1)
	return c.Client.Create(ctx, obj, opts...)
}

func (c *benchmarkRequestCountingClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	c.requestCount.Add(1)
	return c.Client.Delete(ctx, obj, opts...)
}

func (c *benchmarkRequestCountingClient) Update(ctx context.Context, obj client.Object, opts ...client.UpdateOption) error {
	c.requestCount.Add(1)
	return c.Client.Update(ctx, obj, opts...)
}

func (c *benchmarkRequestCountingClient) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.PatchOption) error {
	c.requestCount.Add(1)
	return c.Client.Patch(ctx, obj, patch, opts...)
}

func (c *benchmarkRequestCountingClient) DeleteAllOf(ctx context.Context, obj client.Object, opts ...client.DeleteAllOfOption) error {
	c.requestCount.Add(1)
	return c.Client.DeleteAllOf(ctx, obj, opts...)
}

func (c *benchmarkRequestCountingClient) Status() client.SubResourceWriter {
	return &benchmarkRequestCountingStatusWriter{
		SubResourceWriter: c.Client.Status(),
		requestCount:      &c.requestCount,
	}
}

func (c *benchmarkRequestCountingClient) Reset() {
	c.requestCount.Store(0)
}

func (c *benchmarkRequestCountingClient) Count() int64 {
	return c.requestCount.Load()
}

type benchmarkRequestCountingStatusWriter struct {
	client.SubResourceWriter
	requestCount *atomic.Int64
}

func (w *benchmarkRequestCountingStatusWriter) Create(ctx context.Context, obj client.Object, subResource client.Object, opts ...client.SubResourceCreateOption) error {
	w.requestCount.Add(1)
	return w.SubResourceWriter.Create(ctx, obj, subResource, opts...)
}

func (w *benchmarkRequestCountingStatusWriter) Update(ctx context.Context, obj client.Object, opts ...client.SubResourceUpdateOption) error {
	w.requestCount.Add(1)
	return w.SubResourceWriter.Update(ctx, obj, opts...)
}

func (w *benchmarkRequestCountingStatusWriter) Patch(ctx context.Context, obj client.Object, patch client.Patch, opts ...client.SubResourcePatchOption) error {
	w.requestCount.Add(1)
	return w.SubResourceWriter.Patch(ctx, obj, patch, opts...)
}

type discardEventRecorder struct{}

func (discardEventRecorder) Event(runtime.Object, string, string, string) {}

func (discardEventRecorder) Eventf(runtime.Object, string, string, string, ...interface{}) {}

func (discardEventRecorder) AnnotatedEventf(runtime.Object, map[string]string, string, string, string, ...interface{}) {
}

func BenchmarkPollPodStatus(b *testing.B) {
	response := podStatusResponse{
		Role:              "master",
		ReplicationOffset: 12345,
		ConnectedReplicas: 2,
		Connected:         true,
	}

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/status" {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(response)
	}))
	defer server.Close()

	httpClient := &http.Client{}
	url := server.URL + "/v1/status"
	ctx := context.Background()

	b.ReportAllocs()
	b.ResetTimer()

	for i := 0; i < b.N; i++ {
		status, err := pollPodStatus(ctx, httpClient, url)
		if err != nil {
			b.Fatalf("pollPodStatus returned error: %v", err)
		}
		if !status.Connected {
			b.Fatalf("expected connected status")
		}
	}
}

func BenchmarkPollInstanceStatuses(b *testing.B) {
	for _, clusterCount := range []int{10, 50, 100} {
		clusterCount := clusterCount
		b.Run(fmt.Sprintf("clusters=%d", clusterCount), func(b *testing.B) {
			reconciler, clusters, _ := benchmarkReconcilerWithPods(clusterCount, benchmarkInstancesPerCluster)
			roundTripper := &benchmarkStatusRoundTripper{responseBody: benchmarkPodStatusResponse}

			originalTransport := http.DefaultTransport
			http.DefaultTransport = roundTripper
			b.Cleanup(func() {
				http.DefaultTransport = originalTransport
			})

			ctx := context.Background()
			expectedStatusCount := int(benchmarkInstancesPerCluster)

			b.ReportAllocs()
			b.ResetTimer()
			roundTripper.Reset()

			for i := 0; i < b.N; i++ {
				for _, cluster := range clusters {
					statuses, err := reconciler.pollInstanceStatuses(ctx, cluster)
					if err != nil {
						b.Fatalf("pollInstanceStatuses returned error: %v", err)
					}
					if len(statuses) != expectedStatusCount {
						b.Fatalf("expected %d statuses, got %d", expectedStatusCount, len(statuses))
					}
				}
			}

			elapsedSeconds := b.Elapsed().Seconds()
			if elapsedSeconds > 0 {
				pollsPerSecond := float64(roundTripper.Count()) / elapsedSeconds
				reconcilesPerSecond := float64(b.N*len(clusters)) / elapsedSeconds
				b.ReportMetric(pollsPerSecond, "status_polls/s")
				b.ReportMetric(reconcilesPerSecond, "reconciles/s")
				// pollInstanceStatuses performs one pod list call per reconcile.
				b.ReportMetric(reconcilesPerSecond, "apiserver_list/s")
			}
		})
	}
}

func BenchmarkReconcileLoop(b *testing.B) {
	for _, clusterCount := range []int{10, 50, 100} {
		clusterCount := clusterCount
		b.Run(fmt.Sprintf("clusters=%d", clusterCount), func(b *testing.B) {
			reconciler, clusters, countingClient := benchmarkReconcilerWithPods(clusterCount, benchmarkInstancesPerCluster)
			roundTripper := &benchmarkStatusRoundTripper{responseBody: benchmarkPodStatusResponse}

			originalTransport := http.DefaultTransport
			http.DefaultTransport = roundTripper
			b.Cleanup(func() {
				http.DefaultTransport = originalTransport
			})

			requests := make([]reconcile.Request, 0, len(clusters))
			for _, cluster := range clusters {
				requests = append(requests, reconcile.Request{
					NamespacedName: types.NamespacedName{
						Name:      cluster.Name,
						Namespace: cluster.Namespace,
					},
				})
			}

			ctx := context.Background()

			// Warm up to create or patch initial resources before timed iterations.
			for _, req := range requests {
				if _, err := reconciler.Reconcile(ctx, req); err != nil {
					b.Fatalf("warm-up reconcile failed for %s: %v", req.NamespacedName, err)
				}
			}

			b.ReportAllocs()
			countingClient.Reset()
			roundTripper.Reset()
			b.ResetTimer()

			for i := 0; i < b.N; i++ {
				for _, req := range requests {
					if _, err := reconciler.Reconcile(ctx, req); err != nil {
						b.Fatalf("reconcile failed for %s: %v", req.NamespacedName, err)
					}
				}
			}

			elapsedSeconds := b.Elapsed().Seconds()
			if elapsedSeconds > 0 {
				totalReconciles := float64(b.N * len(requests))
				b.ReportMetric(totalReconciles/elapsedSeconds, "reconciles/s")
				b.ReportMetric(float64(roundTripper.Count())/elapsedSeconds, "status_polls/s")
				b.ReportMetric(float64(countingClient.Count())/elapsedSeconds, "apiserver_calls/s")
			}
		})
	}
}

func benchmarkReconcilerWithPods(clusterCount int, instances int32) (*ClusterReconciler, []*redisv1.RedisCluster, *benchmarkRequestCountingClient) {
	objs := make([]client.Object, 0, clusterCount*int(instances+1))
	clusters := make([]*redisv1.RedisCluster, 0, clusterCount)
	operatorImage := os.Getenv("OPERATOR_IMAGE_NAME")
	if operatorImage == "" {
		operatorImage = defaultOperatorImage
	}
	hashCalculator := &ClusterReconciler{OperatorImage: operatorImage}

	for clusterIndex := 0; clusterIndex < clusterCount; clusterIndex++ {
		clusterName := fmt.Sprintf("bench-%d", clusterIndex)
		cluster := newTestCluster(clusterName, "default", instances)
		cluster.Status.CurrentPrimary = podNameForIndex(clusterName, 0)
		clusters = append(clusters, cluster)
		objs = append(objs, cluster)
		desiredHash := hashCalculator.computeSpecHash(cluster)

		for podIndex := int32(0); podIndex < instances; podIndex++ {
			role := redisv1.LabelRoleReplica
			if podIndex == 0 {
				role = redisv1.LabelRolePrimary
			}
			podName := podNameForIndex(clusterName, int(podIndex))
			objs = append(objs, &corev1.Pod{
				ObjectMeta: metav1.ObjectMeta{
					Name:      podName,
					Namespace: "default",
					Labels:    podLabels(clusterName, podName, role),
					Annotations: map[string]string{
						specHashAnnotation: desiredHash,
					},
				},
				Status: corev1.PodStatus{
					PodIP: fmt.Sprintf("10.%d.%d.%d", (clusterIndex/250)+1, (clusterIndex%250)+1, podIndex+10),
				},
			})
		}
	}

	scheme := testScheme()
	baseClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisCluster{}).
		WithObjects(objs...).
		Build()

	countingClient := &benchmarkRequestCountingClient{Client: baseClient}
	reconciler := NewClusterReconciler(countingClient, scheme, discardEventRecorder{}, 0)
	return reconciler, clusters, countingClient
}
