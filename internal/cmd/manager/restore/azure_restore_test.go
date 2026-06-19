package restore

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
)

func TestResolveAzureBlobLocation(t *testing.T) {
	tests := []struct {
		name          string
		container     string
		backupPath    string
		wantContainer string
		wantBlob      string
		wantErr       bool
	}{
		{
			name:          "azblob url",
			container:     "backups",
			backupPath:    "azblob://backups/redis/backup-1.rdb",
			wantContainer: "backups",
			wantBlob:      "redis/backup-1.rdb",
		},
		{
			name:          "azblob url infers container",
			container:     "",
			backupPath:    "azblob://backups/redis/backup-1.rdb",
			wantContainer: "backups",
			wantBlob:      "redis/backup-1.rdb",
		},
		{
			name:          "plain key with configured container",
			container:     "backups",
			backupPath:    "redis/backup-1.rdb",
			wantContainer: "backups",
			wantBlob:      "redis/backup-1.rdb",
		},
		{
			name:       "container mismatch",
			container:  "other",
			backupPath: "azblob://backups/redis/backup-1.rdb",
			wantErr:    true,
		},
		{
			name:       "empty path",
			container:  "backups",
			backupPath: "",
			wantErr:    true,
		},
		{
			name:       "plain key without container",
			container:  "",
			backupPath: "redis/backup-1.rdb",
			wantErr:    true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			container, blob, err := resolveAzureBlobLocation(tt.container, tt.backupPath)
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantContainer, container)
			assert.Equal(t, tt.wantBlob, blob)
		})
	}
}

func TestReadAzureCredentials(t *testing.T) {
	secret := &corev1.Secret{
		Data: map[string][]byte{
			"AZURE_STORAGE_ACCOUNT": []byte("acct"),
			"AZURE_STORAGE_KEY":     []byte("a2V5"),
		},
	}

	connectionString, accountName, accountKey, sasToken := readAzureCredentials(secret)
	assert.Empty(t, connectionString)
	assert.Equal(t, "acct", accountName)
	assert.Equal(t, "a2V5", accountKey)
	assert.Empty(t, sasToken)
}
