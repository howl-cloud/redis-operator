package backup

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func TestSummarizeBackupMetrics(t *testing.T) {
	completedAtOne := metav1.NewTime(time.Unix(1700000000, 0))
	completedAtTwo := metav1.NewTime(time.Unix(1700001000, 0))

	tests := []struct {
		name                  string
		backups               []redisv1.RedisBackup
		expectedPhaseCounts   map[string]float64
		expectedLastSucceeded float64
	}{
		{
			name: "counts known phases and tracks latest success timestamp",
			backups: []redisv1.RedisBackup{
				{Status: redisv1.RedisBackupStatus{Phase: redisv1.BackupPhasePending}},
				{Status: redisv1.RedisBackupStatus{Phase: redisv1.BackupPhaseRunning}},
				{Status: redisv1.RedisBackupStatus{Phase: redisv1.BackupPhaseCompleted, CompletedAt: &completedAtOne}},
				{Status: redisv1.RedisBackupStatus{Phase: redisv1.BackupPhaseCompleted, CompletedAt: &completedAtTwo}},
				{Status: redisv1.RedisBackupStatus{Phase: redisv1.BackupPhaseFailed}},
			},
			expectedPhaseCounts: map[string]float64{
				string(redisv1.BackupPhasePending):   1,
				string(redisv1.BackupPhaseRunning):   1,
				string(redisv1.BackupPhaseCompleted): 2,
				string(redisv1.BackupPhaseFailed):    1,
				unknownBackupPhase:                   0,
			},
			expectedLastSucceeded: float64(completedAtTwo.Unix()),
		},
		{
			name: "normalizes unknown and empty phases",
			backups: []redisv1.RedisBackup{
				{Status: redisv1.RedisBackupStatus{}},
				{Status: redisv1.RedisBackupStatus{Phase: redisv1.BackupPhase("Unexpected")}},
			},
			expectedPhaseCounts: map[string]float64{
				string(redisv1.BackupPhasePending):   0,
				string(redisv1.BackupPhaseRunning):   0,
				string(redisv1.BackupPhaseCompleted): 0,
				string(redisv1.BackupPhaseFailed):    0,
				unknownBackupPhase:                   2,
			},
			expectedLastSucceeded: 0,
		},
		{
			name: "returns zero timestamp with no completed backups",
			backups: []redisv1.RedisBackup{
				{Status: redisv1.RedisBackupStatus{Phase: redisv1.BackupPhasePending}},
				{Status: redisv1.RedisBackupStatus{Phase: redisv1.BackupPhaseFailed}},
			},
			expectedPhaseCounts: map[string]float64{
				string(redisv1.BackupPhasePending):   1,
				string(redisv1.BackupPhaseRunning):   0,
				string(redisv1.BackupPhaseCompleted): 0,
				string(redisv1.BackupPhaseFailed):    1,
				unknownBackupPhase:                   0,
			},
			expectedLastSucceeded: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phaseCounts, lastSucceeded := summarizeBackupMetrics(tt.backups)

			assert.Equal(t, tt.expectedLastSucceeded, lastSucceeded)
			for _, phase := range knownBackupPhases {
				assert.Equal(t, tt.expectedPhaseCounts[phase], phaseCounts[phase], "phase %s", phase)
			}
		})
	}
}
