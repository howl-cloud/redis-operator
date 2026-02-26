//go:build integration

package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	garageImage                 = "dxflrs/garage:v1.2.0"
	garageS3Port                = "3900/tcp"
	garageAdminPort             = "3903/tcp"
	garageRegion                = "garage"
	garageConfigPath            = "/etc/garage.toml"
	garageAdminToken            = "garage-test-admin-token"
	garageRPCSecret             = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	garageProvisioningTimeout   = 45 * time.Second
	garageProvisioningPollEvery = 250 * time.Millisecond
)

// GarageInfo contains S3 connection details for the test Garage instance.
type GarageInfo struct {
	Endpoint        string
	AccessKeyID     string
	SecretAccessKey string
	Bucket          string
	Region          string
}

type garageStatusResponse struct {
	Node string `json:"node"`
}

type garageKeyResponse struct {
	AccessKeyID     string `json:"accessKeyId"`
	SecretAccessKey string `json:"secretAccessKey"`
}

type garageBucketResponse struct {
	ID string `json:"id"`
}

func startGarageContainer(ctx context.Context, t *testing.T) GarageInfo {
	t.Helper()

	configFilePath := writeGarageConfig(t)

	container, err := testcontainers.Run(
		ctx,
		garageImage,
		testcontainers.WithExposedPorts(garageS3Port, garageAdminPort),
		testcontainers.WithMounts(
			testcontainers.BindMount(configFilePath, testcontainers.ContainerMountTarget(garageConfigPath)),
		),
		testcontainers.WithCmd("/garage", "server"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort(garageAdminPort).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)

	adminPort, err := container.MappedPort(ctx, garageAdminPort)
	require.NoError(t, err)

	s3Port, err := container.MappedPort(ctx, garageS3Port)
	require.NoError(t, err)

	adminURL := fmt.Sprintf("http://%s:%s", host, adminPort.Port())
	s3Endpoint := fmt.Sprintf("http://%s:%s", host, s3Port.Port())

	httpClient := &http.Client{Timeout: 5 * time.Second}

	require.NoError(t, waitForGarageHealth(ctx, httpClient, adminURL))

	garageInfo, err := provisionGarage(ctx, httpClient, adminURL)
	require.NoError(t, err)

	garageInfo.Endpoint = s3Endpoint
	garageInfo.Region = garageRegion

	return garageInfo
}

func writeGarageConfig(t *testing.T) string {
	t.Helper()

	configDir := t.TempDir()
	configPath := filepath.Join(configDir, "garage.toml")
	configContent := fmt.Sprintf(`metadata_dir = "/tmp/meta"
data_dir = "/tmp/data"
db_engine = "sqlite"

replication_factor = 1

rpc_bind_addr = "[::]:3901"
rpc_public_addr = "127.0.0.1:3901"
rpc_secret = "%s"

[s3_api]
s3_region = "%s"
api_bind_addr = "[::]:3900"

[admin]
api_bind_addr = "[::]:3903"
admin_token = "%s"
`, garageRPCSecret, garageRegion, garageAdminToken)
	require.NoError(t, os.WriteFile(configPath, []byte(configContent), 0o600))

	return configPath
}

func waitForGarageHealth(ctx context.Context, httpClient *http.Client, adminURL string) error {
	deadline := time.Now().Add(garageProvisioningTimeout)
	var lastErr error

	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		request, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(adminURL, "/")+"/v1/health", nil)
		if err != nil {
			return fmt.Errorf("creating garage health request: %w", err)
		}
		request.Header.Set("Authorization", "Bearer "+garageAdminToken)

		response, err := httpClient.Do(request)
		if err == nil {
			body, readErr := io.ReadAll(response.Body)
			closeErr := response.Body.Close()
			if readErr != nil {
				lastErr = fmt.Errorf("reading health response: %w", readErr)
			} else if closeErr != nil {
				lastErr = fmt.Errorf("closing health response body: %w", closeErr)
			} else if response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices {
				return nil
			} else {
				lastErr = fmt.Errorf("garage health returned status %d: %s", response.StatusCode, strings.TrimSpace(string(body)))
			}
		} else {
			lastErr = fmt.Errorf("calling garage health endpoint: %w", err)
		}

		time.Sleep(garageProvisioningPollEvery)
	}

	return fmt.Errorf("waiting for Garage health check timed out: %w", lastErr)
}

func provisionGarage(ctx context.Context, httpClient *http.Client, adminURL string) (GarageInfo, error) {
	var statusResponse garageStatusResponse
	if err := garageAdminJSONRequest(ctx, httpClient, adminURL, http.MethodGet, "/v1/status", nil, &statusResponse); err != nil {
		return GarageInfo{}, fmt.Errorf("getting Garage status: %w", err)
	}
	if statusResponse.Node == "" {
		return GarageInfo{}, fmt.Errorf("Garage status response did not include node ID")
	}

	layoutAssignment := []map[string]any{
		{
			"id":       statusResponse.Node,
			"zone":     "dc1",
			"capacity": int64(1 << 30),
			"tags":     []string{},
		},
	}
	if err := garageAdminJSONRequest(ctx, httpClient, adminURL, http.MethodPost, "/v1/layout", layoutAssignment, nil); err != nil {
		return GarageInfo{}, fmt.Errorf("assigning Garage layout: %w", err)
	}

	if err := garageAdminJSONRequest(ctx, httpClient, adminURL, http.MethodPost, "/v1/layout/apply", map[string]any{"version": 1}, nil); err != nil {
		return GarageInfo{}, fmt.Errorf("applying Garage layout: %w", err)
	}

	uniqueSuffix := fmt.Sprintf("%d", time.Now().UnixNano())
	keyName := "redis-operator-key-" + uniqueSuffix
	bucketName := "redis-operator-backups-" + uniqueSuffix

	var keyResponse garageKeyResponse
	if err := garageAdminJSONRequest(
		ctx,
		httpClient,
		adminURL,
		http.MethodPost,
		"/v1/key",
		map[string]any{"name": keyName},
		&keyResponse,
	); err != nil {
		return GarageInfo{}, fmt.Errorf("creating Garage access key %q: %w", keyName, err)
	}
	if keyResponse.AccessKeyID == "" || keyResponse.SecretAccessKey == "" {
		return GarageInfo{}, fmt.Errorf("Garage key response is missing access key credentials")
	}

	var bucketResponse garageBucketResponse
	if err := garageAdminJSONRequest(
		ctx,
		httpClient,
		adminURL,
		http.MethodPost,
		"/v1/bucket",
		map[string]any{"globalAlias": bucketName},
		&bucketResponse,
	); err != nil {
		return GarageInfo{}, fmt.Errorf("creating Garage bucket %q: %w", bucketName, err)
	}
	if bucketResponse.ID == "" {
		return GarageInfo{}, fmt.Errorf("Garage bucket response is missing bucket ID")
	}

	allowRequest := map[string]any{
		"bucketId":    bucketResponse.ID,
		"accessKeyId": keyResponse.AccessKeyID,
		"permissions": map[string]any{
			"read":  true,
			"write": true,
			"owner": true,
		},
	}
	if err := garageAdminJSONRequest(ctx, httpClient, adminURL, http.MethodPost, "/v1/bucket/allow", allowRequest, nil); err != nil {
		return GarageInfo{}, fmt.Errorf("granting Garage bucket permissions: %w", err)
	}

	return GarageInfo{
		AccessKeyID:     keyResponse.AccessKeyID,
		SecretAccessKey: keyResponse.SecretAccessKey,
		Bucket:          bucketName,
		Region:          garageRegion,
	}, nil
}

func garageAdminJSONRequest(
	ctx context.Context,
	httpClient *http.Client,
	adminURL, method, route string,
	requestBody any,
	responseBody any,
) error {
	requestURL := strings.TrimRight(adminURL, "/") + route

	var bodyReader io.Reader
	if requestBody != nil {
		payload, err := json.Marshal(requestBody)
		if err != nil {
			return fmt.Errorf("marshalling request body: %w", err)
		}
		bodyReader = bytes.NewReader(payload)
	}

	request, err := http.NewRequestWithContext(ctx, method, requestURL, bodyReader)
	if err != nil {
		return fmt.Errorf("creating request %s %s: %w", method, requestURL, err)
	}
	request.Header.Set("Authorization", "Bearer "+garageAdminToken)
	if requestBody != nil {
		request.Header.Set("Content-Type", "application/json")
	}

	response, err := httpClient.Do(request)
	if err != nil {
		return fmt.Errorf("calling %s %s: %w", method, requestURL, err)
	}
	defer response.Body.Close() //nolint:errcheck // test helper

	responsePayload, err := io.ReadAll(response.Body)
	if err != nil {
		return fmt.Errorf("reading response body from %s %s: %w", method, requestURL, err)
	}

	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return fmt.Errorf(
			"%s %s returned status %d: %s",
			method,
			requestURL,
			response.StatusCode,
			strings.TrimSpace(string(responsePayload)),
		)
	}

	if responseBody != nil {
		if err := json.Unmarshal(responsePayload, responseBody); err != nil {
			return fmt.Errorf("decoding response body from %s %s: %w", method, requestURL, err)
		}
	}

	return nil
}

func newGarageS3Client(ctx context.Context, garageInfo GarageInfo) (*s3.Client, error) {
	awsConfig, err := config.LoadDefaultConfig(
		ctx,
		config.WithRegion(garageInfo.Region),
		config.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(
			garageInfo.AccessKeyID,
			garageInfo.SecretAccessKey,
			"",
		)),
	)
	if err != nil {
		return nil, fmt.Errorf("loading AWS config: %w", err)
	}

	s3Client := s3.NewFromConfig(awsConfig, func(options *s3.Options) {
		options.BaseEndpoint = aws.String(strings.TrimRight(garageInfo.Endpoint, "/"))
		options.UsePathStyle = true
	})

	return s3Client, nil
}

func uploadFileToS3(ctx context.Context, garageInfo GarageInfo, objectKey, localPath string) error {
	if strings.TrimSpace(objectKey) == "" {
		return fmt.Errorf("object key is required")
	}
	if strings.TrimSpace(localPath) == "" {
		return fmt.Errorf("local path is required")
	}

	s3Client, err := newGarageS3Client(ctx, garageInfo)
	if err != nil {
		return fmt.Errorf("creating S3 client: %w", err)
	}

	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", localPath, err)
	}
	defer file.Close() //nolint:errcheck // read-only close is best effort in tests

	if _, err := s3Client.PutObject(ctx, &s3.PutObjectInput{
		Bucket: aws.String(garageInfo.Bucket),
		Key:    aws.String(objectKey),
		Body:   file,
	}); err != nil {
		return fmt.Errorf("put object s3://%s/%s: %w", garageInfo.Bucket, objectKey, err)
	}

	return nil
}

func downloadFileFromS3(ctx context.Context, garageInfo GarageInfo, objectKey, localPath string) error {
	if strings.TrimSpace(objectKey) == "" {
		return fmt.Errorf("object key is required")
	}
	if strings.TrimSpace(localPath) == "" {
		return fmt.Errorf("local path is required")
	}

	s3Client, err := newGarageS3Client(ctx, garageInfo)
	if err != nil {
		return fmt.Errorf("creating S3 client: %w", err)
	}

	response, err := s3Client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(garageInfo.Bucket),
		Key:    aws.String(objectKey),
	})
	if err != nil {
		return fmt.Errorf("get object s3://%s/%s: %w", garageInfo.Bucket, objectKey, err)
	}
	defer response.Body.Close() //nolint:errcheck // read-only close is best effort in tests

	if err := os.MkdirAll(filepath.Dir(localPath), 0o755); err != nil {
		return fmt.Errorf("creating destination directory for %s: %w", localPath, err)
	}

	tempPath := localPath + ".tmp"
	file, err := os.OpenFile(tempPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return fmt.Errorf("opening temporary destination file %s: %w", tempPath, err)
	}

	if _, err := io.Copy(file, response.Body); err != nil {
		_ = file.Close()
		_ = os.Remove(tempPath)
		return fmt.Errorf("copying object body to %s: %w", tempPath, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("closing temporary destination file %s: %w", tempPath, err)
	}

	if err := os.Rename(tempPath, localPath); err != nil {
		_ = os.Remove(tempPath)
		return fmt.Errorf("renaming %s to %s: %w", tempPath, localPath, err)
	}

	return nil
}
