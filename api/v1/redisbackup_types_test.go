package v1

import "testing"

func TestBackupDestinationValidate(t *testing.T) {
	tests := []struct {
		name        string
		destination *BackupDestination
		wantErr     bool
	}{
		{
			name:        "nil destination",
			destination: nil,
			wantErr:     true,
		},
		{
			name:        "no backend set",
			destination: &BackupDestination{},
			wantErr:     true,
		},
		{
			name:        "s3 valid",
			destination: &BackupDestination{S3: &S3Destination{Bucket: "b"}},
			wantErr:     false,
		},
		{
			name:        "s3 missing bucket",
			destination: &BackupDestination{S3: &S3Destination{}},
			wantErr:     true,
		},
		{
			name:        "azure valid",
			destination: &BackupDestination{Azure: &AzureBlobDestination{Container: "c"}},
			wantErr:     false,
		},
		{
			name:        "azure missing container",
			destination: &BackupDestination{Azure: &AzureBlobDestination{}},
			wantErr:     true,
		},
		{
			name: "both backends set",
			destination: &BackupDestination{
				S3:    &S3Destination{Bucket: "b"},
				Azure: &AzureBlobDestination{Container: "c"},
			},
			wantErr: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.destination.Validate()
			if (err != nil) != tt.wantErr {
				t.Fatalf("Validate() error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}
}
