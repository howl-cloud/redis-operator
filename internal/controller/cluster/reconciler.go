// Package cluster implements the ClusterReconciler for RedisCluster resources.
package cluster

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

const (
	// requeueInterval is the default requeue interval for periodic reconciliation.
	requeueInterval = 30 * time.Second
	// statusPollTimeout is the timeout for HTTP status polls to instance managers.
	statusPollTimeout = 5 * time.Second
)

// ClusterReconciler reconciles RedisCluster objects.
type ClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// NewClusterReconciler creates a new ClusterReconciler.
func NewClusterReconciler(c client.Client, scheme *runtime.Scheme, recorder record.EventRecorder) *ClusterReconciler {
	return &ClusterReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: recorder,
	}
}

// Reconcile is the main entry point called by the controller-runtime framework.
func (r *ClusterReconciler) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	logger := log.FromContext(ctx).WithValues("rediscluster", req.NamespacedName)

	// Fetch the RedisCluster CR.
	var cluster redisv1.RedisCluster
	if err := r.Get(ctx, req.NamespacedName, &cluster); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	logger.Info("Reconciling RedisCluster", "instances", cluster.Spec.Instances, "phase", cluster.Status.Phase)

	// Record event on first reconciliation (Creating phase).
	if cluster.Status.Phase == "" {
		r.Recorder.Event(&cluster, corev1.EventTypeNormal, "Creating", "Cluster reconciliation started")
	}

	result, err := r.reconcile(ctx, &cluster)
	if err != nil {
		r.Recorder.Eventf(&cluster, corev1.EventTypeWarning, "ReconciliationFailed", "Reconciliation failed: %v", err)
		logger.Error(err, "Reconciliation failed")
		return result, err
	}

	return result, nil
}

// reconcile executes the ordered sub-steps of the reconciliation.
func (r *ClusterReconciler) reconcile(ctx context.Context, cluster *redisv1.RedisCluster) (reconcile.Result, error) {
	// Step 1: Global resources (ServiceAccount, RBAC, ConfigMap, PDB).
	if err := r.reconcileGlobalResources(ctx, cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf("reconciling global resources: %w", err)
	}

	// Step 1.5: Hibernation check (after global resources, before services).
	hibernating, err := r.reconcileHibernation(ctx, cluster)
	if err != nil {
		return reconcile.Result{}, fmt.Errorf("reconciling hibernation: %w", err)
	}
	if hibernating {
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}

	// Step 2: Secret resolution.
	if err := r.reconcileSecrets(ctx, cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf("reconciling secrets: %w", err)
	}

	// Step 3: Services (-leader, -replica, -any).
	if err := r.reconcileServices(ctx, cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf("reconciling services: %w", err)
	}

	// Step 4: HTTP status poll.
	instanceStatuses, err := r.pollInstanceStatuses(ctx, cluster)
	if err != nil {
		// Log but don't fail -- some pods may be unreachable during scaling.
		log.FromContext(ctx).Error(err, "Error polling instance statuses")
	}

	// Step 5: Status update.
	if err := r.updateStatus(ctx, cluster, instanceStatuses); err != nil {
		return reconcile.Result{}, fmt.Errorf("updating status: %w", err)
	}

	// Step 6: Reachability check.
	if requeue := r.checkReachability(ctx, cluster, instanceStatuses); requeue {
		return reconcile.Result{RequeueAfter: requeueInterval}, nil
	}

	// Step 7: PVC reconciliation.
	if err := r.reconcilePVCs(ctx, cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf("reconciling PVCs: %w", err)
	}

	// Step 8: Pod reconciliation (scale up/down, rolling updates).
	if err := r.reconcilePods(ctx, cluster); err != nil {
		return reconcile.Result{}, fmt.Errorf("reconciling pods: %w", err)
	}

	return reconcile.Result{RequeueAfter: requeueInterval}, nil
}

// reconcileGlobalResources ensures ServiceAccount, RBAC, ConfigMap, and PDB exist.
func (r *ClusterReconciler) reconcileGlobalResources(ctx context.Context, cluster *redisv1.RedisCluster) error {
	if err := r.reconcileServiceAccount(ctx, cluster); err != nil {
		return fmt.Errorf("service account: %w", err)
	}
	if err := r.reconcileRBAC(ctx, cluster); err != nil {
		return fmt.Errorf("RBAC: %w", err)
	}
	if err := r.reconcileConfigMap(ctx, cluster); err != nil {
		return fmt.Errorf("ConfigMap: %w", err)
	}
	if err := r.reconcilePDB(ctx, cluster); err != nil {
		return fmt.Errorf("PDB: %w", err)
	}
	return nil
}

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *ClusterReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&redisv1.RedisCluster{}).
		Named("cluster-reconciler").
		Complete(r)
}
