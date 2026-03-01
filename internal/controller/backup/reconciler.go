// Package backup implements reconcilers for RedisBackup and RedisScheduledBackup.
package backup

import (
	"context"
	"fmt"
	"sort"
	"strings"
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

	var backup redisv1.RedisBackup
	if err := r.Get(ctx, req.NamespacedName, &backup); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	if backup.Spec.ClusterName != "" {
		defer func() {
			if err := r.refreshClusterBackupMetrics(ctx, backup.Namespace, backup.Spec.ClusterName); err != nil {
				logger.Error(
					err,
					"failed to refresh backup metrics",
					"namespace", backup.Namespace,
					"cluster", backup.Spec.ClusterName,
				)
			}
		}()
	}

	if backup.Status.Phase == redisv1.BackupPhaseCompleted || backup.Status.Phase == redisv1.BackupPhaseFailed {
		return reconcile.Result{}, nil
	}

	logger.Info("Reconciling RedisBackup", "phase", backup.Status.Phase)

	var cluster redisv1.RedisCluster
	if err := r.Get(ctx, types.NamespacedName{
		Name:      backup.Spec.ClusterName,
		Namespace: backup.Namespace,
	}, &cluster); err != nil {
		r.Recorder.Eventf(&backup, corev1.EventTypeWarning, "BackupFailed", "Cluster %s not found", backup.Spec.ClusterName)
		return reconcile.Result{}, r.setBackupFailed(ctx, &backup, fmt.Sprintf("cluster %s not found: %v", backup.Spec.ClusterName, err))
	}

	if cluster.Status.Phase != redisv1.ClusterPhaseHealthy {
		if err := r.setBackupPending(ctx, &backup); err != nil {
			return reconcile.Result{}, err
		}
		logger.Info("Cluster not healthy, requeuing backup", "cluster-phase", cluster.Status.Phase)
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}

	if backup.Spec.Destination == nil || backup.Spec.Destination.S3 == nil || backup.Spec.Destination.S3.Bucket == "" {
		return reconcile.Result{}, r.setBackupFailed(ctx, &backup, "backup destination.s3.bucket is required")
	}

	currentPhase := backup.Status.Phase
	if cluster.Spec.Mode == redisv1.ClusterModeCluster {
		targets, err := r.selectClusterBackupTargets(ctx, &cluster)
		if err != nil {
			return reconcile.Result{}, r.setBackupFailed(ctx, &backup, fmt.Sprintf("selecting cluster backup targets: %v", err))
		}

		var shardArtifacts map[string]redisv1.ShardBackupArtifact
		if currentPhase == "" || currentPhase == redisv1.BackupPhasePending {
			r.Recorder.Event(&backup, corev1.EventTypeNormal, "BackupStarted", "Cluster backup started")
			shardArtifacts, err = r.startClusterBackup(ctx, &backup, targets)
			if err != nil {
				r.Recorder.Eventf(&backup, corev1.EventTypeWarning, "BackupFailed", "Backup failed: %v", err)
				return reconcile.Result{}, r.setBackupFailed(ctx, &backup, fmt.Sprintf("starting cluster backup: %v", err))
			}
		}
		if currentPhase == redisv1.BackupPhaseRunning {
			return reconcile.Result{RequeueAfter: requeueInterval}, nil
		}
		if err := r.setBackupCompleted(ctx, &backup, nil, shardArtifacts); err != nil {
			return reconcile.Result{}, err
		}
		r.Recorder.Event(&backup, corev1.EventTypeNormal, "BackupCompleted", "Cluster backup completed successfully")
		return reconcile.Result{}, nil
	}

	targetPod, targetIP, err := r.selectTargetPod(ctx, &cluster, backup.Spec.Target)
	if err != nil {
		return reconcile.Result{}, r.setBackupFailed(ctx, &backup, fmt.Sprintf("selecting target pod: %v", err))
	}

	var backupResult *BackupResult
	if currentPhase == "" || currentPhase == redisv1.BackupPhasePending {
		r.Recorder.Eventf(&backup, corev1.EventTypeNormal, "BackupStarted", "Backup started on pod %s", targetPod)
		backupResult, err = r.startBackup(ctx, &backup, targetPod, targetIP)
		if err != nil {
			r.Recorder.Eventf(&backup, corev1.EventTypeWarning, "BackupFailed", "Backup failed: %v", err)
			return reconcile.Result{}, r.setBackupFailed(ctx, &backup, fmt.Sprintf("starting backup: %v", err))
		}
	}
	if currentPhase == redisv1.BackupPhaseRunning {
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}

	if err := r.setBackupCompleted(ctx, &backup, backupResult, nil); err != nil {
		return reconcile.Result{}, err
	}
	r.Recorder.Event(&backup, corev1.EventTypeNormal, "BackupCompleted", "Backup completed successfully")

	return reconcile.Result{}, nil
}

// setBackupPending marks the backup as pending when prerequisites are not met yet.
func (r *BackupReconciler) setBackupPending(ctx context.Context, backup *redisv1.RedisBackup) error {
	if backup.Status.Phase == redisv1.BackupPhasePending {
		return nil
	}
	patch := client.MergeFrom(backup.DeepCopy())
	backup.Status.Phase = redisv1.BackupPhasePending
	backup.Status.Error = ""
	return r.Status().Patch(ctx, backup, patch)
}

// selectTargetPod selects a pod to run the backup on.
func (r *BackupReconciler) selectTargetPod(ctx context.Context, cluster *redisv1.RedisCluster, target redisv1.BackupTarget) (string, string, error) {
	pods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return "", "", fmt.Errorf("listing pods: %w", err)
	}

	if target == redisv1.BackupTargetPreferReplica {
		for _, pod := range pods {
			if pod.Name != cluster.Status.CurrentPrimary && pod.Status.PodIP != "" {
				return pod.Name, pod.Status.PodIP, nil
			}
		}
	}

	for _, pod := range pods {
		if pod.Name == cluster.Status.CurrentPrimary && pod.Status.PodIP != "" {
			return pod.Name, pod.Status.PodIP, nil
		}
	}

	return "", "", fmt.Errorf("no suitable pod found for backup")
}

// startBackup triggers the backup on a pod and returns backup artifact metadata.
func (r *BackupReconciler) startBackup(ctx context.Context, backup *redisv1.RedisBackup, targetPod, targetIP string) (*BackupResult, error) {
	patch := client.MergeFrom(backup.DeepCopy())
	backup.Status.Phase = redisv1.BackupPhaseRunning
	backup.Status.TargetPod = targetPod
	now := metav1.Now()
	backup.Status.StartedAt = &now
	if err := r.Status().Patch(ctx, backup, patch); err != nil {
		return nil, err
	}

	return triggerBackup(ctx, targetIP, backup)
}

func (r *BackupReconciler) startClusterBackup(
	ctx context.Context,
	backup *redisv1.RedisBackup,
	targets map[string]clusterBackupTarget,
) (map[string]redisv1.ShardBackupArtifact, error) {
	targetPodNames := make([]string, 0, len(targets))
	for _, target := range targets {
		targetPodNames = append(targetPodNames, target.PodName)
	}
	sort.Strings(targetPodNames)

	patch := client.MergeFrom(backup.DeepCopy())
	backup.Status.Phase = redisv1.BackupPhaseRunning
	backup.Status.TargetPod = strings.Join(targetPodNames, ",")
	now := metav1.Now()
	backup.Status.StartedAt = &now
	if err := r.Status().Patch(ctx, backup, patch); err != nil {
		return nil, err
	}

	shardNames := make([]string, 0, len(targets))
	for shardName := range targets {
		shardNames = append(shardNames, shardName)
	}
	sort.Strings(shardNames)

	shardArtifacts := make(map[string]redisv1.ShardBackupArtifact, len(targets))
	for _, shardName := range shardNames {
		target := targets[shardName]
		shardBackupName := fmt.Sprintf("%s-%s", backup.Name, shardName)
		result, err := triggerBackupWithOptions(
			ctx,
			target.PodIP,
			shardBackupName,
			backup.Spec.Method,
			backup.Spec.Destination,
		)
		if err != nil {
			return nil, fmt.Errorf("triggering shard backup %s on pod %s: %w", shardName, target.PodName, err)
		}
		shardArtifacts[shardName] = redisv1.ShardBackupArtifact{
			TargetPod:      target.PodName,
			BackupPath:     result.BackupPath,
			BackupSize:     result.BackupSize,
			ArtifactType:   result.ArtifactType,
			ChecksumSHA256: result.ChecksumSHA256,
		}
	}

	return shardArtifacts, nil
}

// setBackupCompleted marks the backup as completed.
func (r *BackupReconciler) setBackupCompleted(
	ctx context.Context,
	backup *redisv1.RedisBackup,
	result *BackupResult,
	shardArtifacts map[string]redisv1.ShardBackupArtifact,
) error {
	patch := client.MergeFrom(backup.DeepCopy())
	backup.Status.Phase = redisv1.BackupPhaseCompleted
	backup.Status.ShardArtifacts = nil
	if result != nil {
		backup.Status.BackupPath = result.BackupPath
		backup.Status.BackupSize = result.BackupSize
		if result.ArtifactType != "" {
			backup.Status.ArtifactType = result.ArtifactType
		}
	} else if len(shardArtifacts) > 0 {
		backup.Status.ShardArtifacts = shardArtifacts
		var totalSize int64
		shardNames := make([]string, 0, len(shardArtifacts))
		for shardName := range shardArtifacts {
			shardNames = append(shardNames, shardName)
		}
		sort.Strings(shardNames)
		backup.Status.BackupPath = shardArtifacts[shardNames[0]].BackupPath
		backup.Status.ArtifactType = shardArtifacts[shardNames[0]].ArtifactType
		for _, shardName := range shardNames {
			totalSize += shardArtifacts[shardName].BackupSize
		}
		backup.Status.BackupSize = totalSize
	}
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

type clusterBackupTarget struct {
	PodName string
	PodIP   string
}

func (r *BackupReconciler) selectClusterBackupTargets(
	ctx context.Context,
	cluster *redisv1.RedisCluster,
) (map[string]clusterBackupTarget, error) {
	pods, err := r.listClusterPods(ctx, cluster)
	if err != nil {
		return nil, fmt.Errorf("listing pods: %w", err)
	}
	podsByName := make(map[string]corev1.Pod, len(pods))
	for _, pod := range pods {
		podsByName[pod.Name] = pod
	}

	targets := make(map[string]clusterBackupTarget)
	for shardName, shardStatus := range cluster.Status.Shards {
		if shardStatus.PrimaryPod == "" {
			continue
		}
		pod, ok := podsByName[shardStatus.PrimaryPod]
		if !ok || pod.Status.PodIP == "" {
			continue
		}
		targets[shardName] = clusterBackupTarget{
			PodName: pod.Name,
			PodIP:   pod.Status.PodIP,
		}
	}

	if len(targets) > 0 {
		return targets, nil
	}

	for _, pod := range pods {
		if pod.Labels[redisv1.LabelRole] == redisv1.LabelRoleSentinel {
			continue
		}
		if pod.Labels[redisv1.LabelShardRole] != redisv1.LabelRolePrimary {
			continue
		}
		shardName := pod.Labels[redisv1.LabelShard]
		if shardName == "" || pod.Status.PodIP == "" {
			continue
		}
		targets[shardName] = clusterBackupTarget{
			PodName: pod.Name,
			PodIP:   pod.Status.PodIP,
		}
	}
	if len(targets) == 0 {
		return nil, fmt.Errorf("no shard primary pods with IP found for cluster backup")
	}

	return targets, nil
}

// SetupWithManager registers the BackupReconciler.
func (r *BackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1.RedisBackup{}).
		Named("backup-reconciler").
		Complete(r)
}
