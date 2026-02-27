package backup

import (
	"context"
	"fmt"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const unknownBackupPhase = "Unknown"

var (
	backupControllerAuto = promauto.With(crmetrics.Registry)

	redisLastSuccessfulBackupTimestamp = backupControllerAuto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_last_successful_backup_timestamp",
			Help: "Unix timestamp of the last successful Redis backup per cluster.",
		},
		[]string{"namespace", "cluster"},
	)
	redisBackupPhaseCount = backupControllerAuto.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "redis_backup_phase_count",
			Help: "Current number of RedisBackup resources by phase.",
		},
		[]string{"namespace", "cluster", "phase"},
	)
)

var knownBackupPhases = []string{
	string(redisv1.BackupPhasePending),
	string(redisv1.BackupPhaseRunning),
	string(redisv1.BackupPhaseCompleted),
	string(redisv1.BackupPhaseFailed),
	unknownBackupPhase,
}

func (r *BackupReconciler) refreshClusterBackupMetrics(ctx context.Context, namespace, clusterName string) error {
	if namespace == "" || clusterName == "" {
		return nil
	}

	var backupList redisv1.RedisBackupList
	if err := r.List(ctx, &backupList, client.InNamespace(namespace)); err != nil {
		return fmt.Errorf("listing RedisBackup resources for metrics: %w", err)
	}

	clusterBackups := make([]redisv1.RedisBackup, 0, len(backupList.Items))
	for _, backup := range backupList.Items {
		if backup.Spec.ClusterName != clusterName {
			continue
		}
		clusterBackups = append(clusterBackups, backup)
	}

	phaseCounts, lastSuccessfulTimestamp := summarizeBackupMetrics(clusterBackups)
	for _, phase := range knownBackupPhases {
		redisBackupPhaseCount.WithLabelValues(namespace, clusterName, phase).Set(phaseCounts[phase])
	}
	redisLastSuccessfulBackupTimestamp.WithLabelValues(namespace, clusterName).Set(lastSuccessfulTimestamp)

	return nil
}

func summarizeBackupMetrics(backups []redisv1.RedisBackup) (map[string]float64, float64) {
	phaseCounts := make(map[string]float64, len(knownBackupPhases))
	for _, phase := range knownBackupPhases {
		phaseCounts[phase] = 0
	}

	lastSuccessfulTimestamp := 0.0
	for _, backup := range backups {
		phase := normalizeBackupPhase(backup.Status.Phase)
		phaseCounts[phase]++

		if backup.Status.Phase != redisv1.BackupPhaseCompleted || backup.Status.CompletedAt == nil {
			continue
		}

		completedAt := float64(backup.Status.CompletedAt.Unix())
		if completedAt > lastSuccessfulTimestamp {
			lastSuccessfulTimestamp = completedAt
		}
	}

	return phaseCounts, lastSuccessfulTimestamp
}

func normalizeBackupPhase(phase redisv1.BackupPhase) string {
	if phase == "" {
		return unknownBackupPhase
	}

	phaseValue := string(phase)
	for _, knownPhase := range knownBackupPhases {
		if phaseValue == knownPhase {
			return phaseValue
		}
	}

	return unknownBackupPhase
}
