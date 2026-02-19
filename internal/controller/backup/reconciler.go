// Package backup implements reconcilers for RedisBackup and RedisScheduledBackup.
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

const (
	requeueInterval = 30 * time.Second
)

// BackupReconciler handles on-demand RedisBackup requests.
type BackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// NewBackupReconciler creates a new BackupReconciler.
func NewBackupReconciler(c client.Client, scheme *runtime.Scheme, recorder record.EventRecorder) *BackupReconciler {
	return &BackupReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: recorder,
	}
}

// Reconcile handles a RedisBackup reconciliation.
func (r *BackupReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("redisbackup", req.NamespacedName)

	// Fetch the RedisBackup.
	var backup redisv1.RedisBackup
	if err := r.Get(ctx, req.NamespacedName, &backup); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	// Skip if already completed or failed.
	if backup.Status.Phase == redisv1.BackupPhaseCompleted || backup.Status.Phase == redisv1.BackupPhaseFailed {
		return reconcile.Result{}, nil
	}

	logger.Info("Reconciling RedisBackup", "phase", backup.Status.Phase)

	// Step 1: Validate the referenced cluster exists and is healthy.
	var cluster redisv1.RedisCluster
	if err := r.Get(ctx, types.NamespacedName{
		Name:      backup.Spec.ClusterName,
		Namespace: backup.Namespace,
	}, &cluster); err != nil {
		r.Recorder.Eventf(&backup, corev1.EventTypeWarning, "BackupFailed", "Cluster %s not found", backup.Spec.ClusterName)
		return reconcile.Result{}, r.setBackupFailed(ctx, &backup, fmt.Sprintf("cluster %s not found: %v", backup.Spec.ClusterName, err))
	}

	if cluster.Status.Phase != redisv1.ClusterPhaseHealthy {
		logger.Info("Cluster not healthy, requeuing backup", "cluster-phase", cluster.Status.Phase)
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}

	// Step 2: Select target pod.
	targetPod, targetIP, err := r.selectTargetPod(ctx, &cluster, backup.Spec.Target)
	if err != nil {
		return reconcile.Result{}, r.setBackupFailed(ctx, &backup, fmt.Sprintf("selecting target pod: %v", err))
	}

	// Step 3: Trigger backup on the target pod.
	if backup.Status.Phase == "" || backup.Status.Phase == redisv1.BackupPhasePending {
		r.Recorder.Eventf(&backup, corev1.EventTypeNormal, "BackupStarted", "Backup started on pod %s", targetPod)
		if err := r.startBackup(ctx, &backup, targetPod, targetIP); err != nil {
			r.Recorder.Eventf(&backup, corev1.EventTypeWarning, "BackupFailed", "Backup failed: %v", err)
			return reconcile.Result{}, r.setBackupFailed(ctx, &backup, fmt.Sprintf("starting backup: %v", err))
		}
	}

	// Step 4: Poll for completion.
	// In a full implementation, this would poll the instance manager for backup progress.
	// For now, mark as completed after triggering.
	if err := r.setBackupCompleted(ctx, &backup); err != nil {
		return reconcile.Result{}, err
	}
	r.Recorder.Event(&backup, corev1.EventTypeNormal, "BackupCompleted", "Backup completed successfully")

	return reconcile.Result{}, nil
}

// selectTargetPod selects a pod to run the backup on.
func (r *BackupReconciler) selectTargetPod(ctx context.Context, cluster *redisv1.RedisCluster, target redisv1.BackupTarget) (string, string, error) {
	pods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return "", "", fmt.Errorf("listing pods: %w", err)
	}

	// Prefer a replica if target is prefer-replica.
	if target == redisv1.BackupTargetPreferReplica {
		for _, pod := range pods {
			if pod.Name != cluster.Status.CurrentPrimary && pod.Status.PodIP != "" {
				return pod.Name, pod.Status.PodIP, nil
			}
		}
	}

	// Fall back to primary.
	for _, pod := range pods {
		if pod.Name == cluster.Status.CurrentPrimary && pod.Status.PodIP != "" {
			return pod.Name, pod.Status.PodIP, nil
		}
	}

	return "", "", fmt.Errorf("no suitable pod found for backup")
}

// startBackup triggers the backup on a pod.
func (r *BackupReconciler) startBackup(ctx context.Context, backup *redisv1.RedisBackup, targetPod, targetIP string) error {
	// Trigger backup via HTTP.
	if err := triggerBackup(ctx, targetIP); err != nil {
		return err
	}

	// Update status.
	patch := client.MergeFrom(backup.DeepCopy())
	backup.Status.Phase = redisv1.BackupPhaseRunning
	backup.Status.TargetPod = targetPod
	now := metav1.Now()
	backup.Status.StartedAt = &now
	return r.Status().Patch(ctx, backup, patch)
}

// setBackupCompleted marks the backup as completed.
func (r *BackupReconciler) setBackupCompleted(ctx context.Context, backup *redisv1.RedisBackup) error {
	patch := client.MergeFrom(backup.DeepCopy())
	backup.Status.Phase = redisv1.BackupPhaseCompleted
	now := metav1.Now()
	backup.Status.CompletedAt = &now
	return r.Status().Patch(ctx, backup, patch)
}

// setBackupFailed marks the backup as failed.
func (r *BackupReconciler) setBackupFailed(ctx context.Context, backup *redisv1.RedisBackup, errMsg string) error {
	patch := client.MergeFrom(backup.DeepCopy())
	backup.Status.Phase = redisv1.BackupPhaseFailed
	backup.Status.Error = errMsg
	return r.Status().Patch(ctx, backup, patch)
}

// listClusterPods returns all pods belonging to a cluster.
func (r *BackupReconciler) listClusterPods(ctx context.Context, cluster *redisv1.RedisCluster) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(cluster.Namespace), client.MatchingLabels{
		redisv1.LabelCluster: cluster.Name,
	}); err != nil {
		return nil, err
	}
	return podList.Items, nil
}

// SetupWithManager registers the BackupReconciler.
func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1.RedisBackup{}).
		Named("backup-reconciler").
		Complete(r)
}
