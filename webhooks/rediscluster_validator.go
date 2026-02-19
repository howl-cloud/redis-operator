package webhooks

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// RedisClusterValidator implements admission.CustomValidator for RedisCluster.
type RedisClusterValidator struct{}

var _ webhook.CustomValidator = &RedisClusterValidator{}

// SetupValidatingWebhookWithManager registers the validating webhook with the manager.
func (v *RedisClusterValidator) SetupValidatingWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&redisv1.RedisCluster{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate validates a RedisCluster on creation.
func (v *RedisClusterValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	cluster, ok := obj.(*redisv1.RedisCluster)
	if !ok {
		return nil, fmt.Errorf("expected RedisCluster, got %T", obj)
	}
	return nil, v.validate(cluster).ToAggregate()
}

// ValidateUpdate validates a RedisCluster on update.
func (v *RedisClusterValidator) ValidateUpdate(_ context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error) {
	oldCluster, ok := oldObj.(*redisv1.RedisCluster)
	if !ok {
		return nil, fmt.Errorf("expected RedisCluster, got %T", oldObj)
	}
	newCluster, ok := newObj.(*redisv1.RedisCluster)
	if !ok {
		return nil, fmt.Errorf("expected RedisCluster, got %T", newObj)
	}

	allErrs := v.validate(newCluster)
	allErrs = append(allErrs, v.validateUpdate(oldCluster, newCluster)...)
	return nil, allErrs.ToAggregate()
}

// ValidateDelete validates a RedisCluster on deletion.
func (v *RedisClusterValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validate checks invariants on a RedisCluster spec.
func (v *RedisClusterValidator) validate(cluster *redisv1.RedisCluster) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

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

	return allErrs
}

// validateUpdate checks immutable fields on update.
func (v *RedisClusterValidator) validateUpdate(oldCluster, newCluster *redisv1.RedisCluster) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Storage is immutable after creation.
	if oldCluster.Spec.Storage.Size.Cmp(newCluster.Spec.Storage.Size) != 0 {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("storage", "size"),
			"storage size is immutable after creation; use the resize flow instead",
		))
	}

	// Mode is immutable after creation.
	if oldCluster.Spec.Mode != newCluster.Spec.Mode {
		allErrs = append(allErrs, field.Forbidden(
			specPath.Child("mode"),
			"mode is immutable after creation",
		))
	}

	return allErrs
}
