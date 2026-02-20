package backup

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// ScheduledBackupReconciler handles RedisScheduledBackup resources.
type ScheduledBackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// NewScheduledBackupReconciler creates a new ScheduledBackupReconciler.
func NewScheduledBackupReconciler(c client.Client, scheme *runtime.Scheme, recorder record.EventRecorder) *ScheduledBackupReconciler {
	return &ScheduledBackupReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: recorder,
	}
}

// Reconcile handles a RedisScheduledBackup reconciliation.
func (r *ScheduledBackupReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("redisscheduledbackup", req.NamespacedName)

	// Fetch the RedisScheduledBackup.
	var scheduled redisv1.RedisScheduledBackup
	if err := r.Get(ctx, req.NamespacedName, &scheduled); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Check if suspended.
	if scheduled.Spec.Suspend != nil && *scheduled.Spec.Suspend {
		logger.Info("Scheduled backup is suspended")
		patch := client.MergeFrom(scheduled.DeepCopy())
		scheduled.Status.Phase = redisv1.ScheduledBackupPhaseSuspended
		if err := r.Status().Patch(ctx, &scheduled, patch); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{}, nil
	}

	logger.Info("Reconciling RedisScheduledBackup", "schedule", scheduled.Spec.Schedule)

	// Validate the referenced cluster exists.
	var cluster redisv1.RedisCluster
	if err := r.Get(ctx, types.NamespacedName{
		Name:      scheduled.Spec.ClusterName,
		Namespace: scheduled.Namespace,
	}, &cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf("cluster %s not found: %w", scheduled.Spec.ClusterName, err)
	}

	// Calculate next schedule time.
	now := time.Now()
	nextSchedule, err := nextScheduleTime(scheduled.Spec.Schedule, scheduled.Status.LastScheduleTime, now)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("parsing schedule: %w", err)
	}

	// Update next schedule time in status.
	nextMeta := metav1.NewTime(nextSchedule)
	statusPatch := client.MergeFrom(scheduled.DeepCopy())
	scheduled.Status.NextScheduleTime = &nextMeta
	scheduled.Status.Phase = redisv1.ScheduledBackupPhaseActive
	if err := r.Status().Patch(ctx, &scheduled, statusPatch); err != nil {
		return reconcile.Result{}, err
	}

	// Check if it's time to create a backup.
	if now.Before(nextSchedule) {
		// Not yet time -- requeue at the next schedule time.
		return reconcile.Result{RequeueAfter: nextSchedule.Sub(now)}, nil
	}

	// Create a RedisBackup.
	backupName := fmt.Sprintf("%s-%d", scheduled.Name, now.Unix())
	backup := &redisv1.RedisBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      backupName,
			Namespace: scheduled.Namespace,
			Labels: map[string]string{
				redisv1.LabelCluster:        scheduled.Spec.ClusterName,
				"redis.io/scheduled-backup": scheduled.Name,
			},
		},
		Spec: redisv1.RedisBackupSpec{
			ClusterName: scheduled.Spec.ClusterName,
			Target:      scheduled.Spec.Target,
			Method:      scheduled.Spec.Method,
			Destination: scheduled.Spec.Destination,
		},
	}

	if err := r.Create(ctx, backup); err != nil {
		return reconcile.Result{}, fmt.Errorf("creating RedisBackup: %w", err)
	}

	r.Recorder.Eventf(&scheduled, corev1.EventTypeNormal, "ScheduledBackupTriggered", "Created backup %s from schedule %s", backupName, scheduled.Spec.Schedule)
	logger.Info("Created RedisBackup", "backup", backupName)

	// Update last schedule time.
	nowMeta := metav1.Now()
	lastPatch := client.MergeFrom(scheduled.DeepCopy())
	scheduled.Status.LastScheduleTime = &nowMeta
	scheduled.Status.LastBackupName = backupName
	if err := r.Status().Patch(ctx, &scheduled, lastPatch); err != nil {
		return reconcile.Result{}, err
	}

	// Cleanup old backups.
	if err := r.cleanupOldBackups(ctx, &scheduled); err != nil {
		logger.Error(err, "Failed to cleanup old backups")
	}

	return reconcile.Result{RequeueAfter: requeueInterval}, nil
}

// nextScheduleTime computes the next time a backup should run.
// Uses a simplified cron parser. In production, use a proper cron library.
func nextScheduleTime(schedule string, lastSchedule *metav1.Time, now time.Time) (time.Time, error) {
	// Simplified: treat schedule as an interval duration for demonstration.
	// A production implementation would use robfig/cron or similar.
	// For now, default to every hour if we can't parse.
	interval := 1 * time.Hour

	if lastSchedule == nil {
		return now, nil // Run immediately if never run before.
	}

	next := lastSchedule.Add(interval)
	if next.Before(now) {
		return now, nil // Overdue, run now.
	}
	return next, nil
}

// cleanupOldBackups removes excess backups beyond the history limits.
func (r *ScheduledBackupReconciler) cleanupOldBackups(ctx context.Context, scheduled *redisv1.RedisScheduledBackup) error {
	// List all backups created by this scheduled backup.
	var backupList redisv1.RedisBackupList
	if err := r.List(ctx, &backupList, client.InNamespace(scheduled.Namespace), client.MatchingLabels{
		"redis.io/scheduled-backup": scheduled.Name,
	}); err != nil {
		return fmt.Errorf("listing backups: %w", err)
	}

	// Count successful and failed backups.
	var successful, failed []redisv1.RedisBackup
	for _, b := range backupList.Items {
		switch b.Status.Phase {
		case redisv1.BackupPhaseCompleted:
			successful = append(successful, b)
		case redisv1.BackupPhaseFailed:
			failed = append(failed, b)
		}
	}

	// Cleanup successful backups.
	successLimit := int32(3)
	if scheduled.Spec.SuccessfulBackupsHistoryLimit != nil {
		successLimit = *scheduled.Spec.SuccessfulBackupsHistoryLimit
	}
	if int32(len(successful)) > successLimit {
		excess := int32(len(successful)) - successLimit
		for i := int32(0); i < excess; i++ {
			if err := r.Delete(ctx, &successful[i]); err != nil {
				return fmt.Errorf("deleting old successful backup: %w", err)
			}
		}
	}

	// Cleanup failed backups.
	failedLimit := int32(3)
	if scheduled.Spec.FailedBackupsHistoryLimit != nil {
		failedLimit = *scheduled.Spec.FailedBackupsHistoryLimit
	}
	if int32(len(failed)) > failedLimit {
		excess := int32(len(failed)) - failedLimit
		for i := int32(0); i < excess; i++ {
			if err := r.Delete(ctx, &failed[i]); err != nil {
				return fmt.Errorf("deleting old failed backup: %w", err)
			}
		}
	}

	return nil
}

// SetupWithManager registers the ScheduledBackupReconciler.
func (r *ScheduledBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1.RedisScheduledBackup{}).
		Named("scheduled-backup-reconciler").
		Complete(r)
}
