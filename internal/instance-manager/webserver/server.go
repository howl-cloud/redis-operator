// Package webserver provides the HTTP server running inside each Redis pod.
package webserver

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/howl-cloud/redis-operator/internal/instance-manager/replication"
)

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

	mu       sync.RWMutex
	redisCmd *exec.Cmd
}

// NewServer creates a new HTTP server.
func NewServer(
	redisClient *redis.Client,
	listenAddr string,
	promoteFunc func(ctx context.Context) error,
	demoteFunc func(ctx context.Context, primaryIP string, port int) error,
) *Server {
	return &Server{
		redisClient: redisClient,
		listenAddr:  listenAddr,
		promoteFunc: promoteFunc,
		demoteFunc:  demoteFunc,
	}
}

// SetRedisCmd sets the current redis-server process for liveness checks.
func (s *Server) SetRedisCmd(cmd *exec.Cmd) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.redisCmd = cmd
}

// Start starts the HTTP server. It blocks until the context is cancelled.
func (s *Server) Start(ctx context.Context) error {
	logger := log.FromContext(ctx)
	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", s.handleHealthz)
	mux.HandleFunc("/readyz", s.handleReadyz)
	mux.HandleFunc("/metrics", s.handleMetrics)
	mux.HandleFunc("/v1/status", s.handleStatus)
	mux.HandleFunc("/v1/promote", s.handlePromote)
	mux.HandleFunc("/v1/demote", s.handleDemote)

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

// handleHealthz checks that redis-server is alive.
func (s *Server) handleHealthz(w http.ResponseWriter, _ *http.Request) {
	s.mu.RLock()
	cmd := s.redisCmd
	s.mu.RUnlock()

	if cmd != nil && cmd.Process != nil {
		// Process is running if we can signal 0
		if cmd.ProcessState == nil {
			w.WriteHeader(http.StatusOK)
			_, _ = fmt.Fprint(w, "ok")
			return
		}
	}
	w.WriteHeader(http.StatusServiceUnavailable)
	_, _ = fmt.Fprint(w, "redis-server not running")
}

// handleReadyz checks that Redis responds to PING and is reachable.
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

// handleStatus returns JSON status for the operator's status collection.
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

// handlePromote issues REPLICAOF NO ONE.
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

// handleDemote issues REPLICAOF <primary> 6379.
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

// handleMetrics exposes Redis metrics in Prometheus exposition format.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	result, err := s.redisClient.Info(ctx, "all").Result()
	if err != nil {
		http.Error(w, fmt.Sprintf("failed to get INFO all: %v", err), http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	writePrometheusMetrics(w, result)
}

// writePrometheusMetrics converts Redis INFO all output to Prometheus exposition format.
func writePrometheusMetrics(w http.ResponseWriter, info string) {
	metrics := map[string]struct {
		help    string
		mtype   string
		infoKey string
	}{
		"redis_connected_clients":        {"Number of client connections", "gauge", "connected_clients"},
		"redis_blocked_clients":          {"Number of clients pending on a blocking call", "gauge", "blocked_clients"},
		"redis_used_memory_bytes":        {"Total memory used by Redis in bytes", "gauge", "used_memory"},
		"redis_used_memory_peak_bytes":   {"Peak memory consumed by Redis in bytes", "gauge", "used_memory_peak"},
		"redis_connected_replicas":       {"Number of connected replicas", "gauge", "connected_slaves"},
		"redis_replication_offset":       {"Replication offset", "gauge", "master_repl_offset"},
		"redis_uptime_seconds":           {"Number of seconds since Redis server start", "gauge", "uptime_in_seconds"},
		"redis_keyspace_hits_total":      {"Number of successful lookup of keys", "counter", "keyspace_hits"},
		"redis_keyspace_misses_total":    {"Number of failed lookup of keys", "counter", "keyspace_misses"},
		"redis_commands_processed_total": {"Total number of commands processed by the server", "counter", "total_commands_processed"},
	}

	// Parse INFO all into a key-value map.
	kvMap := make(map[string]string)
	for _, line := range strings.Split(info, "\r\n") {
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) == 2 {
			kvMap[parts[0]] = parts[1]
		}
	}

	for name, m := range metrics {
		val, ok := kvMap[m.infoKey]
		if !ok {
			continue
		}
		_, _ = fmt.Fprintf(w, "# HELP %s %s\n", name, m.help)
		_, _ = fmt.Fprintf(w, "# TYPE %s %s\n", name, m.mtype)
		_, _ = fmt.Fprintf(w, "%s %s\n", name, val)
	}

	// Role as a numeric gauge.
	if role, ok := kvMap["role"]; ok {
		roleVal := "0"
		if role == "master" {
			roleVal = "1"
		}
		_, _ = fmt.Fprint(w, "# HELP redis_replication_role Replication role (0=replica, 1=primary)\n")
		_, _ = fmt.Fprint(w, "# TYPE redis_replication_role gauge\n")
		_, _ = fmt.Fprintf(w, "redis_replication_role %s\n", roleVal)
	}
}
