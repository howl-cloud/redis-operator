package backup

import (
	"context"
	"fmt"
	"net/http"
	"time"
)

var instanceManagerPort = 8080

// triggerBackup sends POST /v1/backup to the instance manager on the target pod.
func triggerBackup(ctx context.Context, podIP string) error {
	url := fmt.Sprintf("http://%s:%d/v1/backup", podIP, instanceManagerPort)
	httpClient := &http.Client{Timeout: 30 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, nil)
	if err != nil {
		return fmt.Errorf("creating backup request: %w", err)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("POST %s returned %d", url, resp.StatusCode)
	}

	return nil
}
