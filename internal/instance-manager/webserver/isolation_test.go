package webserver

import (
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os/exec"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestHandleHealthz_PrimaryIsolation(t *testing.T) {
	type testCase struct {
		name             string
		role             string
		processRunning   bool
		isolationEnabled bool
		configure        func(t *testing.T, srv *Server, cfg PrimaryIsolationConfig)
		wantStatus       int
		wantBodyContains string
	}

	tests := []testCase{
		{
			name:             "primary fails when API and peers unreachable",
			role:             "master",
			processRunning:   true,
			isolationEnabled: true,
			configure: func(_ *testing.T, srv *Server, _ PrimaryIsolationConfig) {
				srv.mu.Lock()
				srv.cachedPeerTargets = []string{"203.0.113.1"}
				srv.mu.Unlock()
			},
			wantStatus:       http.StatusServiceUnavailable,
			wantBodyContains: "primary isolated: cannot reach API server or any peer",
		},
		{
			name:             "primary stays healthy on API outage without peer cache",
			role:             "master",
			processRunning:   true,
			isolationEnabled: true,
			wantStatus:       http.StatusOK,
			wantBodyContains: "ok",
		},
		{
			name:             "primary stays healthy when API server reachable",
			role:             "master",
			processRunning:   true,
			isolationEnabled: true,
			configure: func(t *testing.T, srv *Server, cfg PrimaryIsolationConfig) {
				cluster := &redisv1.RedisCluster{
					ObjectMeta: metav1.ObjectMeta{
						Name:      cfg.ClusterName,
						Namespace: cfg.Namespace,
					},
					Spec: redisv1.RedisClusterSpec{
						Instances: 3,
					},
				}
				peerPod := &corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Name:      "test-cluster-1",
						Namespace: cfg.Namespace,
						Labels: map[string]string{
							redisv1.LabelCluster:  cfg.ClusterName,
							redisv1.LabelWorkload: redisv1.LabelWorkloadData,
						},
					},
					Status: corev1.PodStatus{
						PodIP: "10.0.0.2",
					},
				}
				srv.SetPrimaryIsolationConfig(newIsolationClient(t, cluster, peerPod), cfg)
			},
			wantStatus:       http.StatusOK,
			wantBodyContains: "ok",
		},
		{
			name:             "primary stays healthy when API fails but peer reachable",
			role:             "master",
			processRunning:   true,
			isolationEnabled: true,
			configure: func(t *testing.T, srv *Server, _ PrimaryIsolationConfig) {
				peerServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if r.URL.Path != "/v1/status" {
						w.WriteHeader(http.StatusNotFound)
						return
					}
					w.WriteHeader(http.StatusOK)
					_, _ = w.Write([]byte(`{"role":"slave","connected":true}`))
				}))
				t.Cleanup(peerServer.Close)

				parsedURL, err := url.Parse(peerServer.URL)
				require.NoError(t, err)
				host, port, err := net.SplitHostPort(parsedURL.Host)
				require.NoError(t, err)
				peerPort, err := strconv.Atoi(port)
				require.NoError(t, err)

				srv.mu.Lock()
				srv.cachedPeerTargets = []string{host}
				srv.peerStatusPort = peerPort
				srv.mu.Unlock()
			},
			wantStatus:       http.StatusOK,
			wantBodyContains: "ok",
		},
		{
			name:             "replica ignores isolation checks",
			role:             "slave",
			processRunning:   true,
			isolationEnabled: true,
			wantStatus:       http.StatusOK,
			wantBodyContains: "ok",
		},
		{
			name:             "disabled isolation ignores API and peer failures",
			role:             "master",
			processRunning:   true,
			isolationEnabled: false,
			wantStatus:       http.StatusOK,
			wantBodyContains: "ok",
		},
		{
			name:             "process not running remains unhealthy",
			role:             "master",
			processRunning:   false,
			isolationEnabled: true,
			wantStatus:       http.StatusServiceUnavailable,
			wantBodyContains: "redis-server not running",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, redisClient := newFakeRedisForStatus(t, tt.role, 1234)
			srv := NewServer(redisClient, ":0", nil, nil)

			cfg := PrimaryIsolationConfig{
				Enabled:          tt.isolationEnabled,
				ClusterName:      "test-cluster",
				Namespace:        "default",
				PodName:          "test-cluster-0",
				APIServerTimeout: 50 * time.Millisecond,
				PeerTimeout:      50 * time.Millisecond,
			}
			// Default to API failure for isolation checks unless overridden by test.
			srv.SetPrimaryIsolationConfig(newIsolationClient(t), cfg)

			if tt.configure != nil {
				tt.configure(t, srv, cfg)
			}
			if tt.processRunning {
				setRunningProcess(t, srv)
			}

			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			w := httptest.NewRecorder()
			srv.handleHealthz(w, req)

			assert.Equal(t, tt.wantStatus, w.Code)
			assert.Contains(t, w.Body.String(), tt.wantBodyContains)
		})
	}
}

func TestCheckAnyPeerReachable_ReturnsOnFirstSuccessfulPeer(t *testing.T) {
	srv := &Server{
		peerTimeout:       500 * time.Millisecond,
		peerStatusPort:    8080,
		cachedPeerTargets: []string{"slow-peer", "fast-peer"},
		peerHTTPClient: &http.Client{
			Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
				switch req.URL.Hostname() {
				case "slow-peer":
					select {
					case <-time.After(400 * time.Millisecond):
						return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}, nil
					case <-req.Context().Done():
						return nil, req.Context().Err()
					}
				case "fast-peer":
					return &http.Response{StatusCode: http.StatusOK, Body: http.NoBody}, nil
				default:
					return &http.Response{StatusCode: http.StatusServiceUnavailable, Body: http.NoBody}, nil
				}
			}),
		},
	}

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	start := time.Now()
	err := srv.checkAnyPeerReachable(req.Context())
	elapsed := time.Since(start)
	require.NoError(t, err)
	assert.Less(t, elapsed, 200*time.Millisecond)
}

type roundTripFunc func(req *http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func newIsolationClient(t *testing.T, objects ...client.Object) client.Client {
	t.Helper()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, redisv1.AddToScheme(scheme))
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objects...).
		Build()
}

func setRunningProcess(t *testing.T, srv *Server) {
	t.Helper()

	cmd := exec.Command("sleep", "60")
	require.NoError(t, cmd.Start())
	srv.SetRedisCmd(cmd)
	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
		}
		_ = cmd.Wait()
	})
}
