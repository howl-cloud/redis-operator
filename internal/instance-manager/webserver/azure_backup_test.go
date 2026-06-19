package webserver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// azuriteConnectionString is Azurite's well-known development connection string.
const azuriteConnectionString = "DefaultEndpointsProtocol=http;AccountName=devstoreaccount1;" +
	"AccountKey=Eby8vdM02xNOcqFlqUwJPLlmEtlCDXJ1OUzFT50uSRZ6IFsuFq2UVErCz4I6tq/K1SZFPTOtr/KBHBeksoGMGw==;" +
	"BlobEndpoint=http://127.0.0.1:10000/devstoreaccount1;"

func TestNewAzureBlobClient_CredentialModes(t *testing.T) {
	tests := []struct {
		name             string
		destination      *redisv1.AzureBlobDestination
		connectionString string
		accountName      string
		accountKey       string
		sasToken         string
		wantErr          bool
	}{
		{
			name:             "connection string",
			destination:      &redisv1.AzureBlobDestination{Container: "c"},
			connectionString: azuriteConnectionString,
		},
		{
			name:        "shared key with account name from spec",
			destination: &redisv1.AzureBlobDestination{Container: "c", AccountName: "acct"},
			accountKey:  "Zm9vYmFyYmF6",
		},
		{
			name:        "shared key with account name from secret",
			destination: &redisv1.AzureBlobDestination{Container: "c"},
			accountName: "acct",
			accountKey:  "Zm9vYmFyYmF6",
		},
		{
			name:        "shared key missing account name entirely",
			destination: &redisv1.AzureBlobDestination{Container: "c"},
			accountKey:  "Zm9vYmFyYmF6",
			wantErr:     true,
		},
		{
			name:        "sas token with endpoint",
			destination: &redisv1.AzureBlobDestination{Container: "c", Endpoint: "http://127.0.0.1:10000/devstoreaccount1"},
			sasToken:    "?sv=2021&sig=abc",
		},
		{
			name:        "sas token without account or endpoint",
			destination: &redisv1.AzureBlobDestination{Container: "c"},
			sasToken:    "sv=2021&sig=abc",
			wantErr:     true,
		},
		{
			name:        "no credentials",
			destination: &redisv1.AzureBlobDestination{Container: "c", AccountName: "acct"},
			wantErr:     true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			client, err := newAzureBlobClient(tt.destination, tt.connectionString, tt.accountName, tt.accountKey, tt.sasToken)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.NotNil(t, client)
		})
	}
}

func TestAzureServiceURL(t *testing.T) {
	endpointURL, err := azureServiceURL("", "http://127.0.0.1:10000/devstoreaccount1")
	require.NoError(t, err)
	assert.Equal(t, "http://127.0.0.1:10000/devstoreaccount1", endpointURL)

	publicURL, err := azureServiceURL("acct", "")
	require.NoError(t, err)
	assert.Equal(t, "https://acct.blob.core.windows.net/", publicURL)

	_, err = azureServiceURL("", "")
	require.Error(t, err)
}

func TestAppendSASToken(t *testing.T) {
	assert.Equal(t, "https://acct.blob.core.windows.net/?sv=1", appendSASToken("https://acct.blob.core.windows.net/", "sv=1"))
	assert.Equal(t, "https://acct.blob.core.windows.net/?sv=1", appendSASToken("https://acct.blob.core.windows.net", "?sv=1"))
}

func TestReadAzureCredentialsFromDir(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AZURE_STORAGE_ACCOUNT"), []byte("acct\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "AZURE_STORAGE_KEY"), []byte("a2V5\n"), 0o600))

	connectionString, accountName, accountKey, sasToken, err := readAzureCredentialsFromDir(dir)
	require.NoError(t, err)
	assert.Empty(t, connectionString)
	assert.Equal(t, "acct", accountName)
	assert.Equal(t, "a2V5", accountKey)
	assert.Empty(t, sasToken)
	assert.True(t, strings.HasPrefix(accountKey, "a2V5"))
}
