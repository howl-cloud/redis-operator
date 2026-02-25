package backup

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

var instanceManagerPort = 8080

// BackupResult contains the backup artifact metadata returned by the instance manager.
type BackupResult struct {
	ArtifactType   redisv1.BackupArtifactType
	BackupPath     string
	BackupSize     int64
	ChecksumSHA256 string
}

// triggerBackup sends POST /v1/backup to the instance manager on the target pod.
func triggerBackup(ctx context.Context, podIP string, backup *redisv1.RedisBackup) (*BackupResult, error) {
	url := fmt.Sprintf("http://%s:%d/v1/backup", podIP, instanceManagerPort)
	httpClient := &http.Client{Timeout: 10 * time.Minute}

	reqBody, err := json.Marshal(map[string]any{
		"backupName":  backup.Name,
		"method":      backup.Spec.Method,
		"destination": backup.Spec.Destination,
	})
	if err != nil {
		return nil, fmt.Errorf("marshalling backup request body: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(reqBody))
	if err != nil {
		return nil, fmt.Errorf("creating backup request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4*1024))
		return nil, fmt.Errorf("POST %s returned %d: %s", url, resp.StatusCode, strings.TrimSpace(string(body)))
	}

	var payload struct {
		ArtifactType   redisv1.BackupArtifactType `json:"artifactType"`
		BackupPath     string                     `json:"backupPath"`
		BackupSize     int64                      `json:"backupSize"`
		ChecksumSHA256 string                     `json:"checksumSHA256,omitempty"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decoding backup response: %w", err)
	}
	if payload.BackupPath == "" {
		return nil, fmt.Errorf("instance manager backup response is missing backupPath")
	}

	return &BackupResult{
		ArtifactType:   payload.ArtifactType,
		BackupPath:     payload.BackupPath,
		BackupSize:     payload.BackupSize,
		ChecksumSHA256: payload.ChecksumSHA256,
	}, nil
}
