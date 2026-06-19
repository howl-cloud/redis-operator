//go:build integration

package integration

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob/bloberror"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	azuriteImage = "mcr.microsoft.com/azure-storage/azurite:3.33.0"
	azuritePort  = "10000/tcp"
	// Azurite's well-known development account credentials.
	azuriteAccountName = "devstoreaccount1"
	azuriteAccountKey  = "Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw=="
)

// AzuriteInfo contains Azure Blob connection details for the test Azurite instance.
type AzuriteInfo struct {
	ConnectionString string
	Endpoint         string
	AccountName      string
	AccountKey       string
	Container        string
}

func startAzuriteContainer(ctx context.Context, t *testing.T) AzuriteInfo {
	t.Helper()

	container, err := testcontainers.Run(
		ctx,
		azuriteImage,
		testcontainers.WithExposedPorts(azuritePort),
		testcontainers.WithCmd("azurite-blob", "--blobHost", "0.0.0.0", "--blobPort", "10000", "--skipApiVersionCheck"),
		testcontainers.WithWaitStrategy(
			wait.ForListeningPort(azuritePort).WithStartupTimeout(60*time.Second),
		),
	)
	require.NoError(t, err)
	t.Cleanup(func() {
		_ = container.Terminate(context.Background())
	})

	host, err := container.Host(ctx)
	require.NoError(t, err)

	mappedPort, err := container.MappedPort(ctx, azuritePort)
	require.NoError(t, err)

	endpoint := fmt.Sprintf("http://%s:%s/%s", host, mappedPort.Port(), azuriteAccountName)
	connectionString := fmt.Sprintf(
		"DefaultEndpointsProtocol=http;AccountName=%s;AccountKey=%s;BlobEndpoint=%s;",
		azuriteAccountName, azuriteAccountKey, endpoint,
	)

	containerName := fmt.Sprintf("redis-operator-backups-%d", time.Now().UnixNano())
	client, err := azblob.NewClientFromConnectionString(connectionString, nil)
	require.NoError(t, err)

	require.NoError(t, waitForAzuriteContainerCreate(ctx, client, containerName))

	return AzuriteInfo{
		ConnectionString: connectionString,
		Endpoint:         endpoint,
		AccountName:      azuriteAccountName,
		AccountKey:       azuriteAccountKey,
		Container:        containerName,
	}
}

func waitForAzuriteContainerCreate(ctx context.Context, client *azblob.Client, containerName string) error {
	deadline := time.Now().Add(30 * time.Second)
	var lastErr error
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		_, err := client.CreateContainer(ctx, containerName, nil)
		if err == nil || bloberror.HasCode(err, bloberror.ContainerAlreadyExists) {
			return nil
		}
		lastErr = err
		time.Sleep(250 * time.Millisecond)
	}
	return fmt.Errorf("creating Azurite container %q timed out: %w", containerName, lastErr)
}

func uploadFileToAzure(ctx context.Context, info AzuriteInfo, containerName, blobName, localPath string) error {
	if containerName == "" {
		return errors.New("container name is required")
	}
	if blobName == "" {
		return errors.New("blob name is required")
	}

	client, err := azblob.NewClientFromConnectionString(info.ConnectionString, nil)
	if err != nil {
		return fmt.Errorf("creating azure client: %w", err)
	}

	file, err := os.Open(localPath)
	if err != nil {
		return fmt.Errorf("opening %s: %w", localPath, err)
	}
	defer file.Close() //nolint:errcheck // read-only close is best effort in tests

	if _, err := client.UploadFile(ctx, containerName, blobName, file, nil); err != nil {
		return fmt.Errorf("uploading to azure container %q blob %q: %w", containerName, blobName, err)
	}

	return nil
}
