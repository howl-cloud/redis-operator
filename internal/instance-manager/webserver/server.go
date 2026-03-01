// Package webserver provides the HTTP server running inside each Redis pod.
package webserver

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/redis/go-redis/v9"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
	instmetrics "github.com/howl-cloud/redis-operator/internal/instance-manager/metrics"
	"github.com/howl-cloud/redis-operator/internal/instance-manager/replication"
)

const (
	defaultIsolationTimeout = 5 * time.Second
	defaultPeerStatusPort   = 8080
)

var errNoPeerTargets = errors.New("no peer targets available")

// PrimaryIsolationConfig contains runtime split-brain protection settings.
type PrimaryIsolationConfig struct {
	Enabled          bool
	ClusterName      string
	Namespace        string
	PodName          string
	APIServerTimeout time.Duration
	PeerTimeout      time.Duration
}

// StatusResponse is the JSON response for GET /v1/status.
type StatusResponse struct {
	Role              string `json:"role"`
	ReplicationOffset int64  `json:"replicationOffset"`
	ConnectedReplicas int    `json:"connectedReplicas"`
	MasterLinkStatus  string `json:"masterLinkStatus,omitempty"`
	Connected         bool   `json:"connected"`
}

// Server is the HTTP server for the instance manager.
type Server struct {
	redisClient *redis.Client
	listenAddr  string
	promoteFunc func(ctx context.Context) error
	demoteFunc  func(ctx context.Context, primaryIP string, port int) error
	processName string
	dataDir     string

	mu       sync.RWMutex
	redisCmd *exec.Cmd

	exposeDataEndpoints  bool
	backupCredentialsDir string
	backupUploader       backupUploaderFunc

	k8sClient               client.Client
	primaryIsolationEnabled bool
	clusterName             string
	namespace               string
	podName                 string
	apiServerTimeout        time.Duration
	peerTimeout             time.Duration
	peerStatusPort          int
	peerHTTPClient          *http.Client
	cachedPeerTargets       []string
}

// NewServer creates a new HTTP server.
func NewServer(
	redisClient *redis.Client,
	listenAddr string,
	promoteFunc func(ctx context.Context) error,
	demoteFunc func(ctx context.Context, primaryIP string, port int) error,
) *Server {
	return &Server{
		redisClient:          redisClient,
		listenAddr:           listenAddr,
		promoteFunc:          promoteFunc,
		demoteFunc:           demoteFunc,
		processName:          "redis-server",
		dataDir:              defaultBackupDataDir,
		backupCredentialsDir: defaultBackupCredsMountPath,
		exposeDataEndpoints:  true,
		peerStatusPort:       defaultPeerStatusPort,
	}
}

// NewSentinelServer creates an HTTP server for sentinel pods.
func NewSentinelServer(redisClient *redis.Client, listenAddr string, sentinelCmd *exec.Cmd) *Server {
	srv := &Server{
		redisClient:         redisClient,
		listenAddr:          listenAddr,
		processName:         "redis-sentinel",
		exposeDataEndpoints: false,
		peerStatusPort:      defaultPeerStatusPort,
	}
	srv.SetRedisCmd(sentinelCmd)
	return srv
}

// SetPrimaryIsolationConfig configures runtime primary-isolation checks for /healthz.
func (s *Server) SetPrimaryIsolationConfig(k8sClient client.Client, cfg PrimaryIsolationConfig) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if cfg.APIServerTimeout <= 0 {
		cfg.APIServerTimeout = defaultIsolationTimeout
	}
	if cfg.PeerTimeout <= 0 {
		cfg.PeerTimeout = defaultIsolationTimeout
	}

	s.k8sClient = k8sClient
	s.primaryIsolationEnabled = cfg.Enabled
	s.clusterName = cfg.ClusterName
	s.namespace = cfg.Namespace
	s.podName = cfg.PodName
	s.apiServerTimeout = cfg.APIServerTimeout
	s.peerTimeout = cfg.PeerTimeout
}

// SetRedisCmd sets the current supervised process for liveness checks.
func (s *Server) SetRedisCmd(cmd *exec.Cmd) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redisCmd = cmd
}

// SetMetricsIdentity sets identity labels used for Redis Prometheus metrics.
func (s *Server) SetMetricsIdentity(clusterName, namespace, podName string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.clusterName = clusterName
	s.namespace = namespace
	s.podName = podName
}

// Start starts the HTTP server. It blocks until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	if s.exposeDataEndpoints {
		mux.Handle("/metrics", s.metricsHandler())
		mux.HandleFunc("/v1/status", s.handleStatus)
		mux.HandleFunc("/v1/promote", s.handlePromote)
		mux.HandleFunc("/v1/demote", s.handleDemote)
		mux.HandleFunc("/v1/backup", s.handleBackup)
	}

	srv := &http.Server{
		Addr:              s.listenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			logger.Error(err, "HTTP server shutdown error")
		}
	}()

	logger.Info("Starting HTTP server", "addr", s.listenAddr)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return fmt.Errorf("HTTP server error: %w", err)
	}
	return nil
}

// handleHealthz checks process liveness and runtime primary isolation.
func (s *Server) handleHealthz(w http.ResponseWriter, r *http.Request) {
	s.mu.RLock()
	cmd := s.redisCmd
	s.mu.RUnlock()
	processName := s.processName
	if processName == "" {
		processName = "redis-server"
	}

	if cmd != nil && cmd.Process != nil {
		if cmd.ProcessState == nil {
			if !s.shouldRunPrimaryIsolationCheck(r.Context()) {
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, "ok")
				return
			}

			apiErr := s.checkAPIServerReachable(r.Context())
			if apiErr == nil {
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, "ok")
				return
			}
			peerErr := s.checkAnyPeerReachable(r.Context())
			if peerErr == nil {
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, "ok")
				return
			}
			if errors.Is(peerErr, errNoPeerTargets) {
				// If peer targets are unknown (for example before cache warm-up), do not
				// treat API-only outages as full primary isolation.
				log.FromContext(r.Context()).Info(
					"primary isolation check skipped peer validation: no cached peer targets",
					"pod", s.podName,
					"cluster", s.clusterName,
					"apiError", apiErr,
				)
				w.WriteHeader(http.StatusOK)
				_, _ = fmt.Fprint(w, "ok")
				return
			}

			log.FromContext(r.Context()).Error(
				apiErr,
				"primary isolation check failed",
				"peerError", peerErr,
				"pod", s.podName,
				"cluster", s.clusterName,
			)
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = fmt.Fprint(w, "primary isolated: cannot reach API server or any peer")
			return
		}
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprintf(w, "%s not running", processName)
}

func (s *Server) shouldRunPrimaryIsolationCheck(ctx context.Context) bool {
	s.mu.RLock()
	enabled := s.primaryIsolationEnabled
	exposeDataEndpoints := s.exposeDataEndpoints
	k8sClient := s.k8sClient
	s.mu.RUnlock()

	if !enabled || !exposeDataEndpoints || k8sClient == nil || s.redisClient == nil {
		return false
	}

	info, err := replication.GetInfo(ctx, s.redisClient)
	if err != nil {
		return false
	}
	return info.Role == "master"
}

func (s *Server) checkAPIServerReachable(ctx context.Context) error {
	s.mu.RLock()
	k8sClient := s.k8sClient
	clusterName := s.clusterName
	namespace := s.namespace
	podName := s.podName
	timeout := s.apiServerTimeout
	s.mu.RUnlock()

	if k8sClient == nil {
		return errors.New("kubernetes client not configured")
	}
	if clusterName == "" || namespace == "" {
		return errors.New("primary isolation cluster identity not configured")
	}
	if timeout <= 0 {
		timeout = defaultIsolationTimeout
	}

	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	var cluster redisv1.RedisCluster
	if err := k8sClient.Get(checkCtx, types.NamespacedName{
		Name:      clusterName,
		Namespace: namespace,
	}, &cluster); err != nil {
		return err
	}

	peerTargets, err := listPeerTargetsFromAPI(checkCtx, k8sClient, clusterName, namespace, podName)
	s.mu.Lock()
	if err == nil {
		s.cachedPeerTargets = peerTargets
	}
	s.mu.Unlock()

	if err != nil {
		log.FromContext(ctx).Error(err, "failed to refresh peer cache from API")
	}
	return nil
}

func listPeerTargetsFromAPI(
	ctx context.Context,
	k8sClient client.Client,
	clusterName, namespace, podName string,
) ([]string, error) {
	var pods corev1.PodList
	if err := k8sClient.List(ctx, &pods, client.InNamespace(namespace), client.MatchingLabels{
		redisv1.LabelCluster:  clusterName,
		redisv1.LabelWorkload: redisv1.LabelWorkloadData,
	}); err != nil {
		return nil, err
	}

	targets := make([]string, 0, len(pods.Items))
	for _, pod := range pods.Items {
		if pod.Name == podName || pod.Status.PodIP == "" {
			continue
		}
		targets = append(targets, pod.Status.PodIP)
	}

	sort.Strings(targets)
	return dedupeStrings(targets), nil
}

func dedupeStrings(values []string) []string {
	if len(values) <= 1 {
		return values
	}
	out := make([]string, 0, len(values))
	for _, v := range values {
		if len(out) == 0 || out[len(out)-1] != v {
			out = append(out, v)
		}
	}
	return out
}

func (s *Server) checkAnyPeerReachable(ctx context.Context) error {
	targets := s.peerTargetsForIsolation()
	if len(targets) == 0 {
		return errNoPeerTargets
	}

	s.mu.RLock()
	timeout := s.peerTimeout
	statusPort := s.peerStatusPort
	httpClient := s.peerHTTPClient
	s.mu.RUnlock()

	if timeout <= 0 {
		timeout = defaultIsolationTimeout
	}
	if statusPort <= 0 {
		statusPort = defaultPeerStatusPort
	}
	if httpClient == nil {
		httpClient = &http.Client{Timeout: timeout}
	}

	peerCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	type peerCheckResult struct {
		err error
	}
	results := make(chan peerCheckResult, len(targets))

	for _, target := range targets {
		target := target
		go func() {
			req, err := http.NewRequestWithContext(
				peerCtx,
				http.MethodGet,
				fmt.Sprintf("http://%s:%d/v1/status", target, statusPort),
				nil,
			)
			if err != nil {
				results <- peerCheckResult{err: err}
				return
			}

			resp, err := httpClient.Do(req)
			if err != nil {
				results <- peerCheckResult{err: err}
				return
			}
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				results <- peerCheckResult{}
				return
			}

			results <- peerCheckResult{err: fmt.Errorf("peer %s returned status %d", target, resp.StatusCode)}
		}()
	}

	var lastErr error
	for i := 0; i < len(targets); i++ {
		select {
		case <-peerCtx.Done():
			if lastErr != nil {
				return lastErr
			}
			return peerCtx.Err()
		case result := <-results:
			if result.err == nil {
				cancel()
				return nil
			}
			lastErr = result.err
		}
	}
	if lastErr == nil {
		lastErr = errors.New("no peer targets responded")
	}
	return lastErr
}

func (s *Server) peerTargetsForIsolation() []string {
	s.mu.RLock()
	cachedTargets := append([]string(nil), s.cachedPeerTargets...)
	s.mu.RUnlock()

	return cachedTargets
}

func (s *Server) metricsHandler() http.Handler {
	s.mu.RLock()
	collector := instmetrics.NewRedisCollector(
		s.redisClient,
		s.namespace,
		s.clusterName,
		s.podName,
		s.isCurrentPodFenced,
	)
	s.mu.RUnlock()

	registry := prometheus.NewRegistry()
	if err := registry.Register(collector); err != nil {
		emptyRegistry := prometheus.NewRegistry()
		return promhttp.HandlerFor(emptyRegistry, promhttp.HandlerOpts{})
	}
	return promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
}

func (s *Server) isCurrentPodFenced(ctx context.Context) (bool, error) {
	s.mu.RLock()
	k8sClient := s.k8sClient
	clusterName := s.clusterName
	namespace := s.namespace
	podName := s.podName
	s.mu.RUnlock()

	if k8sClient == nil || clusterName == "" || namespace == "" || podName == "" {
		return false, nil
	}

	var cluster redisv1.RedisCluster
	if err := k8sClient.Get(ctx, types.NamespacedName{Name: clusterName, Namespace: namespace}, &cluster); err != nil {
		return false, err
	}
	return podIsFenced(&cluster, podName), nil
}

func podIsFenced(cluster *redisv1.RedisCluster, podName string) bool {
	if cluster == nil || podName == "" {
		return false
	}
	if cluster.Annotations == nil {
		return false
	}

	raw := cluster.Annotations[redisv1.FencingAnnotationKey]
	if raw == "" {
		return false
	}

	var fencedPods []string
	if err := json.Unmarshal([]byte(raw), &fencedPods); err != nil {
		return false
	}
	for _, fencedPod := range fencedPods {
		if fencedPod == podName {
			return true
		}
	}
	return false
}

func (s *Server) handleReadyz(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	if err := s.redisClient.Ping(ctx).Err(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = fmt.Fprintf(w, "redis not ready: %v", err)
		return
	}
	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "ok")
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	ctx := r.Context()
	info, err := replication.GetInfo(ctx, s.redisClient)
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get replication info: %v", err), http.StatusInternalServerError)
		return
	}

	resp := StatusResponse{
		Role:              info.Role,
		ReplicationOffset: info.MasterReplOffset,
		ConnectedReplicas: info.ConnectedReplicas,
		MasterLinkStatus:  info.MasterLinkStatus,
		Connected:         true,
	}
	if info.Role == "slave" {
		resp.ReplicationOffset = info.SlaveReplOffset
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

func (s *Server) handlePromote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	if err := s.promoteFunc(r.Context()); err != nil {
		http.Error(w, fmt.Sprintf("promote failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "promoted")
}

func (s *Server) handleDemote(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}

	primaryIP := r.URL.Query().Get("primaryIP")
	portStr := r.URL.Query().Get("port")
	if primaryIP == "" {
		http.Error(w, "missing primaryIP query parameter", http.StatusBadRequest)
		return
	}
	port := 6379
	if portStr != "" {
		var err error
		port, err = strconv.Atoi(portStr)
		if err != nil {
			http.Error(w, "invalid port", http.StatusBadRequest)
			return
		}
	}

	if err := s.demoteFunc(r.Context(), primaryIP, port); err != nil {
		http.Error(w, fmt.Sprintf("demote failed: %v", err), http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
	_, _ = fmt.Fprint(w, "demoted")
}
