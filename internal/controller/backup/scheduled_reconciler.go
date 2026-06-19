package backup

import (
	"context"
	"fmt"
	"time"

	cron "github.com/robfig/cron/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
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

	var scheduled redisv1.RedisScheduledBackup
	if err := r.Get(ctx, req.NamespacedName, &scheduled); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

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

	var cluster redisv1.RedisCluster
	if err := r.Get(ctx, types.NamespacedName{
		Name:      scheduled.Spec.ClusterName,
		Namespace: scheduled.Namespace,
	}, &cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf("cluster %s not found: %w", scheduled.Spec.ClusterName, err)
	}

	sched, loc, err := parseSchedule(scheduled.Spec.Schedule, scheduled.Spec.TimeZone)
	if err != nil {
		logger.Error(err, "Invalid schedule; waiting for spec update")
		return r.markScheduleInvalid(ctx, &scheduled, err)
	}

	now := time.Now()
	next := sched.Next(baselineTime(&scheduled).In(loc))

	// A syntactically valid expression can still have no reachable occurrence
	// (e.g. "0 0 31 2 *" — February 31st). robfig/cron signals this by returning
	// the zero time. Treat it as invalid: firing on a zero "next" would create a
	// backup and then requeue every second forever.
	if next.IsZero() {
		err := fmt.Errorf("schedule %q has no upcoming occurrences", scheduled.Spec.Schedule)
		logger.Error(err, "Unreachable schedule; waiting for spec update")
		return r.markScheduleInvalid(ctx, &scheduled, err)
	}

	// Not due yet: record the next wall-clock time and requeue for it.
	if next.After(now) {
		patch := client.MergeFrom(scheduled.DeepCopy())
		nextMeta := metav1.NewTime(next)
		scheduled.Status.NextScheduleTime = &nextMeta
		scheduled.Status.Phase = redisv1.ScheduledBackupPhaseActive
		meta.SetStatusCondition(&scheduled.Status.Conditions, scheduleValidCondition(scheduled.Generation))
		if err := r.Status().Patch(ctx, &scheduled, patch); err != nil {
			return reconcile.Result{}, err
		}
		return reconcile.Result{RequeueAfter: requeueDelay(next, now)}, nil
	}

	// Due — including catch-up after controller downtime. Fire exactly one backup
	// regardless of how many windows were missed, then advance the schedule past
	// `now`, so missed runs never stack up.
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

	nextRun := sched.Next(now.In(loc))
	nowMeta := metav1.NewTime(now)
	nextMeta := metav1.NewTime(nextRun)
	lastPatch := client.MergeFrom(scheduled.DeepCopy())
	scheduled.Status.LastScheduleTime = &nowMeta
	scheduled.Status.LastBackupName = backupName
	scheduled.Status.NextScheduleTime = &nextMeta
	scheduled.Status.Phase = redisv1.ScheduledBackupPhaseActive
	meta.SetStatusCondition(&scheduled.Status.Conditions, scheduleValidCondition(scheduled.Generation))
	if err := r.Status().Patch(ctx, &scheduled, lastPatch); err != nil {
		return reconcile.Result{}, err
	}

	if err := r.cleanupOldBackups(ctx, &scheduled); err != nil {
		logger.Error(err, "Failed to cleanup old backups")
	}

	return reconcile.Result{RequeueAfter: requeueDelay(nextRun, now)}, nil
}

// markScheduleInvalid records that the schedule cannot be honored: it sets the
// ScheduleValid=False condition, emits a warning event, and returns an empty
// result so the controller stops requeuing. The schedule is part of the spec, so
// a fix only arrives via a spec update, which re-triggers reconciliation on its
// own. The validating webhook normally rejects these, but this guards the case
// where the webhook is disabled.
func (r *ScheduledBackupReconciler) markScheduleInvalid(ctx context.Context, scheduled *redisv1.RedisScheduledBackup, cause error) (reconcile.Result, error) {
	patch := client.MergeFrom(scheduled.DeepCopy())
	meta.SetStatusCondition(&scheduled.Status.Conditions, metav1.Condition{
		Type:               redisv1.ScheduledBackupConditionScheduleValid,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: scheduled.Generation,
		Reason:             "InvalidSchedule",
		Message:            cause.Error(),
	})
	scheduled.Status.Phase = redisv1.ScheduledBackupPhaseActive
	if err := r.Status().Patch(ctx, scheduled, patch); err != nil {
		return reconcile.Result{}, err
	}
	r.Recorder.Eventf(scheduled, corev1.EventTypeWarning, "InvalidSchedule", "Cannot schedule backups: %v", cause)
	return reconcile.Result{}, nil
}

// baselineTime returns the reference point for computing the next scheduled run:
// the last time a backup was triggered, or the resource creation time if none
// has run yet. Anchoring on the creation time means the first backup fires at
// the next cron occurrence rather than immediately on creation.
func baselineTime(scheduled *redisv1.RedisScheduledBackup) time.Time {
	if scheduled.Status.LastScheduleTime != nil {
		return scheduled.Status.LastScheduleTime.Time
	}
	return scheduled.CreationTimestamp.Time
}

// parseSchedule parses the cron expression and resolves the time zone (defaulting
// to UTC). Standard 5-field cron and the @hourly/@daily/@weekly/@monthly/@yearly
// and @every <duration> descriptors are supported; seconds are not.
func parseSchedule(schedule, timeZone string) (cron.Schedule, *time.Location, error) {
	tz := timeZone
	if tz == "" {
		tz = "UTC"
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid timeZone %q: %w", tz, err)
	}
	sched, err := cron.ParseStandard(schedule)
	if err != nil {
		return nil, nil, fmt.Errorf("invalid schedule %q: %w", schedule, err)
	}
	return sched, loc, nil
}

// requeueDelay returns how long to wait before the next reconcile, clamped to a
// one-second minimum so a sub-second @every schedule cannot spin the controller.
func requeueDelay(next, now time.Time) time.Duration {
	if d := next.Sub(now); d > time.Second {
		return d
	}
	return time.Second
}

// scheduleValidCondition reports that the schedule parsed successfully.
func scheduleValidCondition(generation int64) metav1.Condition {
	return metav1.Condition{
		Type:               redisv1.ScheduledBackupConditionScheduleValid,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: generation,
		Reason:             "ScheduleParsed",
		Message:            "Schedule parsed successfully",
	}
}

// cleanupOldBackups removes excess backups beyond the history limits.
func (r *ScheduledBackupReconciler) cleanupOldBackups(ctx context.Context, scheduled *redisv1.RedisScheduledBackup) error {
	var backupList redisv1.RedisBackupList
	if err := r.List(ctx, &backupList, client.InNamespace(scheduled.Namespace), client.MatchingLabels{
		"redis.io/scheduled-backup": scheduled.Name,
	}); err != nil {
		return fmt.Errorf("listing backups: %w", err)
	}

	var successful, failed []redisv1.RedisBackup
	for _, b := range backupList.Items {
		switch b.Status.Phase {
		case redisv1.BackupPhaseCompleted:
			successful = append(successful, b)
		case redisv1.BackupPhaseFailed:
			failed = append(failed, b)
		}
	}

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
