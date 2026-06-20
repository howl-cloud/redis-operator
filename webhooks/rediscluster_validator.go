package webhooks

import (
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/validation"
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
	sentinelInstancesMinMessage      = "sentinel mode requires at least 3 redis instances"
	sentinelTLSUnsupported           = "TLS is not supported in sentinel mode yet"
	clusterShardsMinMessage          = "cluster mode requires spec.shards >= 3"
	clusterInstancesForbiddenMessage = "spec.instances is not allowed in cluster mode; use shards and replicasPerShard"
	clusterMinSyncUnsupportedMessage = "minSyncReplicas is not supported in cluster mode"
	clusterMaxSyncUnsupportedMessage = "maxSyncReplicas is not supported in cluster mode"
	clusterReplicaModeUnsupported    = "replicaMode is not supported in cluster mode"
	tlsSecretRequiredMessage         = "tlsSecret is required when caSecret is set"
	caSecretRequiredMessage          = "caSecret is required when tlsSecret is set"
	primaryUpdateApprovalValue       = `must be "true" when present`
	maintenanceSingleInstanceMessage = "nodeMaintenanceWindow.reusePVC=false is not allowed for single-instance clusters; set reusePVC=true or scale up first"
	replicaModeSourceRequiredMessage = "source is required when replicaMode.enabled=true"
	replicaModeHostRequiredMessage   = "source.host is required when replicaMode.enabled=true"
	replicaModePromoteMessage        = "replicaMode.promote=true requires replicaMode.enabled=true"
	replicaModeDisableMessage        = "replicaMode.enabled cannot be disabled directly; set replicaMode.promote=true first"
	ephemeralBootstrapMessage        = "bootstrap is not supported with ephemeral storage (storage.type=emptyDir); restored data would be lost when pods are recreated"
	ephemeralStorageClassMessage     = "storageClassName has no effect with ephemeral storage (storage.type=emptyDir); remove it"
	storageTypeImmutableMessage      = "storage.type is immutable after creation; switching between pvc and emptyDir would destroy or migrate data"
	memoryRedisConflictMessage       = "configure Redis memory via spec.memory; remove maxmemory/maxmemory-policy from spec.redis"
	memoryMutualExclusiveMessage     = "spec.memory.maxMemory and spec.memory.maxMemoryPercent are mutually exclusive; set only one"
	memoryPercentRequiresLimit       = "spec.memory.maxMemoryPercent requires spec.resources.limits.memory to be set"
	memoryExceedsLimitMessage        = "spec.memory.maxMemory must not exceed spec.resources.limits.memory"
	memoryNonPositiveMessage         = "spec.memory.maxMemory must be greater than 0"
	modeTransitionUnsupportedMessage = "unsupported mode transition; only standalone->sentinel is supported as an in-place migration (set instances>=3 and do not enable TLS). See docs/runbooks/standalone-to-sentinel-migration.md"
	modeMigrationNotReadyMessage     = "cannot migrate standalone->sentinel until status.currentPrimary is set; wait for the cluster to become Healthy before changing mode"
	modeMigrationTLSMessage          = "cannot migrate standalone->sentinel in place for a TLS-enabled cluster (sentinel mode does not support TLS yet); clearing tlsSecret/caSecret in the same change would strand the migration. Use the backup/recreate path. See docs/runbooks/standalone-to-sentinel-migration.md"
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
	if err := v.validate(ctx, cluster).ToAggregate(); err != nil {
		return nil, err
	}
	return memoryWarnings(cluster), nil
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
	if err := allErrs.ToAggregate(); err != nil {
		return nil, err
	}
	return memoryWarnings(newCluster), nil
}

// ValidateDelete validates a RedisCluster on deletion.
func (v *RedisClusterValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validate checks invariants on a RedisCluster spec.
func (v *RedisClusterValidator) validate(ctx context.Context, cluster *redisv1.RedisCluster) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

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

	if val, ok := cluster.Annotations[redisv1.AnnotationApprovePrimaryUpdate]; ok {
		if val != "true" && val != "" {
			allErrs = append(allErrs, field.Invalid(
				field.NewPath("metadata", "annotations", redisv1.AnnotationApprovePrimaryUpdate),
				val,
				primaryUpdateApprovalValue,
			))
		}
	}

	if cluster.Spec.Mode == redisv1.ClusterModeCluster {
		if cluster.Spec.Shards < 3 {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("shards"),
				cluster.Spec.Shards,
				clusterShardsMinMessage,
			))
		}
		if cluster.Spec.Instances != 0 {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("instances"),
				cluster.Spec.Instances,
				clusterInstancesForbiddenMessage,
			))
		}
		if cluster.Spec.MinSyncReplicas != 0 {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("minSyncReplicas"),
				cluster.Spec.MinSyncReplicas,
				clusterMinSyncUnsupportedMessage,
			))
		}
		if cluster.Spec.MaxSyncReplicas != 0 {
			allErrs = append(allErrs, field.Invalid(
				specPath.Child("maxSyncReplicas"),
				cluster.Spec.MaxSyncReplicas,
				clusterMaxSyncUnsupportedMessage,
			))
		}
		if cluster.Spec.ReplicaMode != nil {
			allErrs = append(allErrs, field.Forbidden(
				specPath.Child("replicaMode"),
				clusterReplicaModeUnsupported,
			))
		}
	} else if cluster.Spec.Instances < 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("instances"),
			cluster.Spec.Instances,
			"must be at least 1",
		))
	}

	if cluster.Spec.Mode != redisv1.ClusterModeCluster && cluster.Spec.MinSyncReplicas > cluster.Spec.Instances-1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("minSyncReplicas"),
			cluster.Spec.MinSyncReplicas,
			"must be less than or equal to instances - 1",
		))
	}

	if cluster.Spec.Mode != redisv1.ClusterModeCluster && cluster.Spec.MaxSyncReplicas < cluster.Spec.MinSyncReplicas {
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
		cluster.Spec.DesiredDataInstances() == 1 {
		allErrs = append(allErrs, field.Invalid(
			specPath.Child("nodeMaintenanceWindow", "reusePVC"),
			*cluster.Spec.NodeMaintenanceWindow.ReusePVC,
			maintenanceSingleInstanceMessage,
		))
	}

	if cluster.Spec.Mode != redisv1.ClusterModeCluster {
		allErrs = append(allErrs, v.validateReplicaMode(ctx, cluster)...)
	}
	allErrs = append(allErrs, validateConnectionSecret(cluster)...)
	allErrs = append(allErrs, v.validateBootstrapReference(ctx, cluster)...)
	allErrs = append(allErrs, validateStorage(cluster)...)
	allErrs = append(allErrs, validateMemory(cluster)...)

	return allErrs
}

// validateMemory checks spec.memory for unsafe or inconsistent combinations.
func validateMemory(cluster *redisv1.RedisCluster) field.ErrorList {
	var allErrs field.ErrorList
	mem := cluster.Spec.Memory

	// Raw spec.redis keys must not coexist with first-class spec.memory.
	if mem != nil {
		redisPath := field.NewPath("spec", "redis")
		for key := range cluster.Spec.Redis {
			normalized := strings.ToLower(strings.TrimSpace(key))
			if normalized == "maxmemory" || normalized == "maxmemory-policy" {
				allErrs = append(allErrs, field.Forbidden(redisPath.Key(key), memoryRedisConflictMessage))
			}
		}
	}

	if mem == nil {
		return allErrs
	}

	memPath := field.NewPath("spec", "memory")
	limit := cluster.Spec.MemoryLimitBytes()

	if mem.MaxMemory != nil && mem.MaxMemoryPercent != nil {
		allErrs = append(allErrs, field.Forbidden(memPath.Child("maxMemoryPercent"), memoryMutualExclusiveMessage))
	}

	if mem.MaxMemoryPercent != nil && limit <= 0 {
		allErrs = append(allErrs, field.Invalid(
			memPath.Child("maxMemoryPercent"), *mem.MaxMemoryPercent, memoryPercentRequiresLimit))
	}

	if mem.MaxMemory != nil {
		switch v := mem.MaxMemory.Value(); {
		case v <= 0:
			allErrs = append(allErrs, field.Invalid(
				memPath.Child("maxMemory"), mem.MaxMemory.String(), memoryNonPositiveMessage))
		case limit > 0 && v > limit:
			allErrs = append(allErrs, field.Invalid(
				memPath.Child("maxMemory"), mem.MaxMemory.String(), memoryExceedsLimitMessage))
		}
	}

	return allErrs
}

// memoryWarnings surfaces non-fatal memory configuration risks: a container
// memory limit with no maxmemory (OOM-kill risk), or a maxmemory left with too
// little headroom under the limit.
func memoryWarnings(cluster *redisv1.RedisCluster) admission.Warnings {
	limit := cluster.Spec.MemoryLimitBytes()
	if limit <= 0 {
		return nil
	}

	bytes, ok := cluster.Spec.ResolveMaxMemoryBytes()
	if !ok {
		if hasRawMaxMemory(cluster) {
			return nil
		}
		return admission.Warnings{fmt.Sprintf(
			"spec.resources.limits.memory is set (%d bytes) but no Redis maxmemory is configured; "+
				"Redis may be OOM-killed at the container limit instead of applying eviction. "+
				"Set spec.memory.maxMemory or spec.memory.maxMemoryPercent.", limit)}
	}

	if bytes*10 >= limit*9 {
		return admission.Warnings{fmt.Sprintf(
			"spec.memory resolves maxmemory to %d bytes, at least 90%% of the %d byte container limit; "+
				"leave headroom for replication buffers, copy-on-write during persistence, and fragmentation.",
			bytes, limit)}
	}

	return nil
}

// hasRawMaxMemory reports whether a non-empty maxmemory is set via spec.redis.
func hasRawMaxMemory(cluster *redisv1.RedisCluster) bool {
	for key, val := range cluster.Spec.Redis {
		if strings.EqualFold(strings.TrimSpace(key), "maxmemory") && strings.TrimSpace(val) != "" {
			return true
		}
	}
	return false
}

// validateConnectionSecret checks that the published connection Secret name, when
// set, is a valid Kubernetes object name (DNS-1123 subdomain).
func validateConnectionSecret(cluster *redisv1.RedisCluster) field.ErrorList {
	if cluster.Spec.ConnectionSecret == nil {
		return nil
	}

	namePath := field.NewPath("spec", "connectionSecret", "name")
	name := strings.TrimSpace(cluster.Spec.ConnectionSecret.Name)
	if name == "" {
		return field.ErrorList{field.Required(namePath, "name is required when connectionSecret is set")}
	}

	var allErrs field.ErrorList
	for _, msg := range validation.IsDNS1123Subdomain(name) {
		allErrs = append(allErrs, field.Invalid(namePath, name, msg))
	}
	return allErrs
}

// validateStorage checks ephemeral-storage constraints. Fields that only make
// sense for PVC-backed storage are rejected when storage.type=emptyDir.
func validateStorage(cluster *redisv1.RedisCluster) field.ErrorList {
	if !cluster.Spec.Storage.IsEphemeral() {
		return nil
	}

	var allErrs field.ErrorList
	storagePath := field.NewPath("spec", "storage")

	if cluster.Spec.Bootstrap != nil && cluster.Spec.Bootstrap.BackupName != "" {
		allErrs = append(allErrs, field.Forbidden(
			field.NewPath("spec", "bootstrap"),
			ephemeralBootstrapMessage,
		))
	}

	if cluster.Spec.Storage.StorageClassName != nil && *cluster.Spec.Storage.StorageClassName != "" {
		allErrs = append(allErrs, field.Forbidden(
			storagePath.Child("storageClassName"),
			ephemeralStorageClassMessage,
		))
	}

	return allErrs
}

// validateUpdate checks immutable fields and storage resize constraints on update.
func (v *RedisClusterValidator) validateUpdate(ctx context.Context, oldCluster, newCluster *redisv1.RedisCluster) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")
	storageSizePath := specPath.Child("storage", "size")

	// storage.type is immutable: switching backends would destroy or migrate data.
	if oldCluster.Spec.Storage.Type != newCluster.Spec.Storage.Type {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("storage", "type"),
			storageTypeImmutableMessage,
		))
	}

	// PVC resize/expansion rules apply only to PVC-backed storage. For ephemeral
	// clusters a size change simply re-bounds the emptyDir on pod recreation.
	if !newCluster.Spec.Storage.IsEphemeral() {
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
	}

	// Mode transitions are restricted. Only standalone->sentinel is supported as
	// an in-place upgrade to high availability; every other change is rejected.
	if oldCluster.Spec.Mode != newCluster.Spec.Mode {
		allErrs = append(allErrs, validateModeTransition(oldCluster, newCluster, specPath)...)
	}

	if bootstrapBackupName(oldCluster.Spec.Bootstrap) != bootstrapBackupName(newCluster.Spec.Bootstrap) {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("bootstrap", "backupName"),
			"bootstrap backupName is immutable after creation",
		))
	}

	oldReplicaEnabled := replicaModeEnabled(oldCluster.Spec.ReplicaMode)
	newReplicaEnabled := replicaModeEnabled(newCluster.Spec.ReplicaMode)
	if oldReplicaEnabled && !newReplicaEnabled {
		oldPromoteRequested := oldCluster.Spec.ReplicaMode != nil && oldCluster.Spec.ReplicaMode.Promote
		if !oldPromoteRequested {
			allErrs = append(allErrs, field.Forbidden(
				specPath.Child("replicaMode", "enabled"),
				replicaModeDisableMessage,
			))
		}
	}

	newPromoteRequested := newCluster.Spec.ReplicaMode != nil && newCluster.Spec.ReplicaMode.Promote
	if !oldReplicaEnabled && newPromoteRequested {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("replicaMode", "promote"),
			"replicaMode.promote can only be set after replicaMode is already enabled",
		))
	}

	return allErrs
}

// validateModeTransition allows only the supported standalone->sentinel in-place
// migration. The target sentinel invariants (instances >= 3, no TLS) are enforced
// by validate(); here we gate on a transition allowlist and require an established
// primary so sentinel has a master to monitor on startup.
func validateModeTransition(oldCluster, newCluster *redisv1.RedisCluster, specPath *field.Path) field.ErrorList {
	modePath := specPath.Child("mode")
	if oldCluster.Spec.Mode == redisv1.ClusterModeStandalone &&
		newCluster.Spec.Mode == redisv1.ClusterModeSentinel {
		if newCluster.Status.CurrentPrimary == "" {
			return field.ErrorList{field.Forbidden(modePath, modeMigrationNotReadyMessage)}
		}
		// A TLS-enabled source cannot migrate in place: sentinel mode has no TLS
		// support, and clearing TLS in the same change would leave the running
		// primary on TLS while new replicas/sentinels come up without it. Checking
		// the old spec (not just the new one) closes the clear-TLS-in-same-patch
		// bypass of the target-spec TLS validation.
		if oldCluster.Spec.TLSSecret != nil || oldCluster.Spec.CASecret != nil {
			return field.ErrorList{field.Forbidden(modePath, modeMigrationTLSMessage)}
		}
		return nil
	}
	return field.ErrorList{field.Forbidden(modePath, modeTransitionUnsupportedMessage)}
}

func (v *RedisClusterValidator) validateReplicaMode(ctx context.Context, cluster *redisv1.RedisCluster) field.ErrorList {
	replicaMode := cluster.Spec.ReplicaMode
	if replicaMode == nil {
		return nil
	}

	var allErrs field.ErrorList
	replicaModePath := field.NewPath("spec", "replicaMode")

	if replicaMode.Promote && !replicaMode.Enabled {
		allErrs = append(allErrs, field.Invalid(
			replicaModePath.Child("promote"),
			replicaMode.Promote,
			replicaModePromoteMessage,
		))
	}

	if !replicaMode.Enabled {
		return allErrs
	}

	if replicaMode.Source == nil {
		allErrs = append(allErrs, field.Required(
			replicaModePath.Child("source"),
			replicaModeSourceRequiredMessage,
		))
		return allErrs
	}

	if strings.TrimSpace(replicaMode.Source.Host) == "" {
		allErrs = append(allErrs, field.Required(
			replicaModePath.Child("source", "host"),
			replicaModeHostRequiredMessage,
		))
	}

	if replicaMode.Source.Port < 0 || replicaMode.Source.Port > 65535 {
		allErrs = append(allErrs, field.Invalid(
			replicaModePath.Child("source", "port"),
			replicaMode.Source.Port,
			"must be between 1 and 65535 when set",
		))
	}

	authSecretName := strings.TrimSpace(replicaMode.Source.AuthSecretName)
	if replicaMode.Source.AuthSecretName != "" && authSecretName == "" {
		allErrs = append(allErrs, field.Invalid(
			replicaModePath.Child("source", "authSecretName"),
			replicaMode.Source.AuthSecretName,
			"must not be empty when set",
		))
	}
	if authSecretName != "" && v.Reader != nil {
		var secret corev1.Secret
		if err := v.Reader.Get(ctx, types.NamespacedName{
			Name:      authSecretName,
			Namespace: cluster.Namespace,
		}, &secret); err != nil {
			if apierrors.IsNotFound(err) {
				allErrs = append(allErrs, field.NotFound(
					replicaModePath.Child("source", "authSecretName"),
					authSecretName,
				))
			} else {
				allErrs = append(allErrs, field.InternalError(
					replicaModePath.Child("source", "authSecretName"),
					fmt.Errorf("getting secret %s/%s: %w", cluster.Namespace, authSecretName, err),
				))
			}
		}
	}

	return allErrs
}

func replicaModeEnabled(replicaMode *redisv1.ReplicaModeSpec) bool {
	return replicaMode != nil && replicaMode.Enabled
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

	if err := backup.Spec.Destination.Validate(); err != nil {
		allErrs = append(allErrs, field.Invalid(
			backupNamePath,
			backupName,
			fmt.Sprintf("referenced RedisBackup has an invalid destination: %v", err),
		))
	}

	if backup.Status.BackupPath == "" && len(backup.Status.ShardArtifacts) == 0 {
		allErrs = append(allErrs, field.Invalid(
			backupNamePath,
			backupName,
			"referenced RedisBackup must have status.backupPath or status.shardArtifacts set",
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
