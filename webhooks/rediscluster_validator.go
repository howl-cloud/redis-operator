package webhooks

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// RedisClusterValidator implements admission.CustomValidator for RedisCluster.
type RedisClusterValidator struct {
	Reader client.Reader
}

var _ webhook.CustomValidator = &RedisClusterValidator{}

const (
	unsupportedModeMessage           = "cluster mode is not yet supported; use standalone or sentinel"
	sentinelInstancesMinMessage      = "sentinel mode requires at least 3 redis instances"
	sentinelTLSUnsupported           = "TLS is not supported in sentinel mode yet"
	tlsSecretRequiredMessage         = "tlsSecret is required when caSecret is set"
	caSecretRequiredMessage          = "caSecret is required when tlsSecret is set"
	primaryUpdateApprovalValue       = `must be "true" when present`
	maintenanceSingleInstanceMessage = "nodeMaintenanceWindow.reusePVC=false is not allowed for single-instance clusters; set reusePVC=true or scale up first"
)

// SetupValidatingWebhookWithManager registers the validating webhook with the manager.
func (v *RedisClusterValidator) SetupValidatingWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&redisv1.RedisCluster{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate validates a RedisCluster on creation.
func (v *RedisClusterValidator) ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error) {
	cluster, ok := obj.(*redisv1.RedisCluster)
	if !ok {
		return nil, fmt.Errorf("expected RedisCluster, got %T", obj)
	}
	return nil, v.validate(ctx, cluster).ToAggregate()
}

// ValidateUpdate validates a RedisCluster on update.
func (v *RedisClusterValidator) ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldCluster, ok := oldObj.(*redisv1.RedisCluster)
	if !ok {
		return nil, fmt.Errorf("expected RedisCluster, got %T", oldObj)
	}
	newCluster, ok := newObj.(*redisv1.RedisCluster)
	if !ok {
		return nil, fmt.Errorf("expected RedisCluster, got %T", newObj)
	}

	allErrs := v.validate(ctx, newCluster)
	allErrs = append(allErrs, v.validateUpdate(ctx, oldCluster, newCluster)...)
	return nil, allErrs.ToAggregate()
}

// ValidateDelete validates a RedisCluster on deletion.
func (v *RedisClusterValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validate checks invariants on a RedisCluster spec.
func (v *RedisClusterValidator) validate(ctx context.Context, cluster *redisv1.RedisCluster) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	if cluster.Spec.Mode == redisv1.ClusterModeCluster {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("mode"),
			cluster.Spec.Mode,
			unsupportedModeMessage,
		))
	}

	if cluster.Spec.Mode == redisv1.ClusterModeSentinel && cluster.Spec.Instances < 3 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("instances"),
			cluster.Spec.Instances,
			sentinelInstancesMinMessage,
		))
	}

	if cluster.Spec.Mode == redisv1.ClusterModeSentinel &&
		(cluster.Spec.TLSSecret != nil || cluster.Spec.CASecret != nil) {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("mode"),
			cluster.Spec.Mode,
			sentinelTLSUnsupported,
		))
	}

	if cluster.Spec.TLSSecret != nil && cluster.Spec.CASecret == nil {
		allErrs = append(allErrs, field.Required(
			specPath.Child("caSecret"),
			caSecretRequiredMessage,
		))
	}

	if cluster.Spec.CASecret != nil && cluster.Spec.TLSSecret == nil {
		allErrs = append(allErrs, field.Required(
			specPath.Child("tlsSecret"),
			tlsSecretRequiredMessage,
		))
	}

	// Validate hibernation annotation value if present.
	if val, ok := cluster.Annotations[redisv1.AnnotationHibernation]; ok {
		validValues := map[string]bool{"on": true, "off": true, "true": true, "false": true, "": true}
		if !validValues[val] {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("metadata", "annotations", redisv1.AnnotationHibernation),
				val,
				`must be "on", "off", "true", "false", or empty`,
			))
		}
	}

	// Validate supervised primary update approval annotation if present.
	if val, ok := cluster.Annotations[redisv1.AnnotationApprovePrimaryUpdate]; ok {
		if val != "true" && val != "" {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("metadata", "annotations", redisv1.AnnotationApprovePrimaryUpdate),
				val,
				primaryUpdateApprovalValue,
			))
		}
	}

	if cluster.Spec.Instances < 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("instances"),
			cluster.Spec.Instances,
			"must be at least 1",
		))
	}

	if cluster.Spec.MinSyncReplicas > cluster.Spec.Instances-1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("minSyncReplicas"),
			cluster.Spec.MinSyncReplicas,
			"must be less than or equal to instances - 1",
		))
	}

	if cluster.Spec.MaxSyncReplicas < cluster.Spec.MinSyncReplicas {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("maxSyncReplicas"),
			cluster.Spec.MaxSyncReplicas,
			"must be greater than or equal to minSyncReplicas",
		))
	}

	if cluster.Spec.NodeMaintenanceWindow != nil &&
		cluster.Spec.NodeMaintenanceWindow.InProgress &&
		cluster.Spec.NodeMaintenanceWindow.ReusePVC != nil &&
		!*cluster.Spec.NodeMaintenanceWindow.ReusePVC &&
		cluster.Spec.Instances == 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("nodeMaintenanceWindow", "reusePVC"),
			*cluster.Spec.NodeMaintenanceWindow.ReusePVC,
			maintenanceSingleInstanceMessage,
		))
	}

	allErrs = append(allErrs, v.validateBootstrapReference(ctx, cluster)...)

	return allErrs
}

// validateUpdate checks immutable fields and storage resize constraints on update.
func (v *RedisClusterValidator) validateUpdate(ctx context.Context, oldCluster, newCluster *redisv1.RedisCluster) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	storageSizePath := specPath.Child("storage", "size")

	switch oldCluster.Spec.Storage.Size.Cmp(newCluster.Spec.Storage.Size) {
	case 1:
		// Allow rollbacks only when they do not shrink any already-requested PVC.
		if v.Reader == nil {
			allErrs = append(allErrs, field.Forbidden(
				storageSizePath,
				"storage size cannot be decreased after creation; only increases are allowed",
			))
			break
		}
		pvcs, err := v.listClusterPVCs(ctx, newCluster)
		if err != nil {
			allErrs = append(allErrs, field.InternalError(storageSizePath, fmt.Errorf("listing cluster PVCs: %w", err)))
			break
		}
		maxRequestedStorage := resource.Quantity{}
		for i := range pvcs {
			requested := pvcs[i].Spec.Resources.Requests[corev1.ResourceStorage]
			if requested.Cmp(maxRequestedStorage) > 0 {
				maxRequestedStorage = requested
			}
		}
		if maxRequestedStorage.Cmp(newCluster.Spec.Storage.Size) > 0 {
			allErrs = append(allErrs, field.Forbidden(
				storageSizePath,
				fmt.Sprintf(
					"storage size cannot be decreased below currently requested PVC size %q",
					maxRequestedStorage.String(),
				),
			))
		}
	case -1:
		expandable, reason, err := v.validateStorageClassExpansion(ctx, newCluster)
		if err != nil {
			allErrs = append(allErrs, field.InternalError(storageSizePath, err))
			break
		}
		if !expandable {
			allErrs = append(allErrs, field.Forbidden(storageSizePath, reason))
		}
	}

	// Mode is immutable after creation.
	if oldCluster.Spec.Mode != newCluster.Spec.Mode {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("mode"),
			"mode is immutable after creation",
		))
	}

	if bootstrapBackupName(oldCluster.Spec.Bootstrap) != bootstrapBackupName(newCluster.Spec.Bootstrap) {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("bootstrap", "backupName"),
			"bootstrap backupName is immutable after creation",
		))
	}

	return allErrs
}

func (v *RedisClusterValidator) validateStorageClassExpansion(
	ctx context.Context,
	cluster *redisv1.RedisCluster,
) (bool, string, error) {
	if v.Reader == nil {
		return true, "", nil
	}

	storageClasses := make(map[string]struct{})
	if cluster.Spec.Storage.StorageClassName != nil && *cluster.Spec.Storage.StorageClassName != "" {
		storageClasses[*cluster.Spec.Storage.StorageClassName] = struct{}{}
	}

	pvcs, err := v.listClusterPVCs(ctx, cluster)
	if err != nil {
		return false, "", fmt.Errorf("listing cluster PVCs: %w", err)
	}
	for i := range pvcs {
		if pvcs[i].Spec.StorageClassName != nil && *pvcs[i].Spec.StorageClassName != "" {
			storageClasses[*pvcs[i].Spec.StorageClassName] = struct{}{}
		}
	}

	for storageClassName := range storageClasses {
		var storageClass storagev1.StorageClass
		if err := v.Reader.Get(ctx, types.NamespacedName{Name: storageClassName}, &storageClass); err != nil {
			if apierrors.IsNotFound(err) {
				return false, fmt.Sprintf("storageClass %q was not found", storageClassName), nil
			}
			return false, "", fmt.Errorf("getting storageClass %q: %w", storageClassName, err)
		}
		if storageClass.AllowVolumeExpansion == nil || !*storageClass.AllowVolumeExpansion {
			return false, fmt.Sprintf("storageClass %q does not allow volume expansion", storageClassName), nil
		}
	}

	return true, "", nil
}

func (v *RedisClusterValidator) listClusterPVCs(ctx context.Context, cluster *redisv1.RedisCluster) ([]corev1.PersistentVolumeClaim, error) {
	var pvcList corev1.PersistentVolumeClaimList
	if err := v.Reader.List(
		ctx,
		&pvcList,
		client.InNamespace(cluster.Namespace),
		client.MatchingLabels{redisv1.LabelCluster: cluster.Name},
	); err != nil {
		return nil, err
	}
	return pvcList.Items, nil
}

func (v *RedisClusterValidator) validateBootstrapReference(ctx context.Context, cluster *redisv1.RedisCluster) field.ErrorList {
	if cluster.Spec.Bootstrap == nil || cluster.Spec.Bootstrap.BackupName == "" {
		return nil
	}

	var allErrs field.ErrorList
	backupNamePath := field.NewPath("spec", "bootstrap", "backupName")
	backupName := cluster.Spec.Bootstrap.BackupName

	if v.Reader == nil {
		allErrs = append(allErrs, field.InternalError(backupNamePath, fmt.Errorf("validator reader is not configured")))
		return allErrs
	}

	var backup redisv1.RedisBackup
	if err := v.Reader.Get(ctx, types.NamespacedName{
		Name:      backupName,
		Namespace: cluster.Namespace,
	}, &backup); err != nil {
		if apierrors.IsNotFound(err) {
			allErrs = append(allErrs, field.NotFound(backupNamePath, backupName))
			return allErrs
		}
		allErrs = append(allErrs, field.InternalError(backupNamePath, fmt.Errorf("fetching RedisBackup %s/%s: %w", cluster.Namespace, backupName, err)))
		return allErrs
	}

	if backup.Status.Phase != redisv1.BackupPhaseCompleted {
		allErrs = append(allErrs, field.Invalid(
			backupNamePath,
			backupName,
			fmt.Sprintf("referenced RedisBackup must be in phase %q", redisv1.BackupPhaseCompleted),
		))
	}

	if backup.Spec.Destination == nil || backup.Spec.Destination.S3 == nil {
		allErrs = append(allErrs, field.Invalid(
			backupNamePath,
			backupName,
			"referenced RedisBackup must define an S3 destination",
		))
	}

	if backup.Status.BackupPath == "" {
		allErrs = append(allErrs, field.Invalid(
			backupNamePath,
			backupName,
			"referenced RedisBackup must have status.backupPath set",
		))
	}

	return allErrs
}

func bootstrapBackupName(spec *redisv1.BootstrapSpec) string {
	if spec == nil {
		return ""
	}
	return spec.BackupName
}
