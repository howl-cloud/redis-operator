package webhooks

import (
	"context"
	"fmt"
	"time"

	cron "github.com/robfig/cron/v3"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

// RedisScheduledBackupValidator implements admission.CustomValidator for
// RedisScheduledBackup. It rejects schedules and time zones that the controller
// would otherwise be unable to parse, giving the user immediate feedback at
// apply time instead of a deferred status condition.
type RedisScheduledBackupValidator struct{}

var _ webhook.CustomValidator = &RedisScheduledBackupValidator{}

// SetupValidatingWebhookWithManager registers the validating webhook with the manager.
func (v *RedisScheduledBackupValidator) SetupValidatingWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&redisv1.RedisScheduledBackup{}).
		WithValidator(v).
		Complete()
}

// ValidateCreate validates a RedisScheduledBackup on creation.
func (v *RedisScheduledBackupValidator) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	scheduled, ok := obj.(*redisv1.RedisScheduledBackup)
	if !ok {
		return nil, fmt.Errorf("expected RedisScheduledBackup, got %T", obj)
	}
	return nil, validateSchedule(scheduled).ToAggregate()
}

// ValidateUpdate validates a RedisScheduledBackup on update.
func (v *RedisScheduledBackupValidator) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	scheduled, ok := newObj.(*redisv1.RedisScheduledBackup)
	if !ok {
		return nil, fmt.Errorf("expected RedisScheduledBackup, got %T", newObj)
	}
	return nil, validateSchedule(scheduled).ToAggregate()
}

// ValidateDelete is a no-op; deletion needs no schedule validation.
func (v *RedisScheduledBackupValidator) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	return nil, nil
}

// validateSchedule checks that spec.schedule is a parseable cron expression and
// that spec.timeZone, when set, names a loadable IANA time zone.
func validateSchedule(scheduled *redisv1.RedisScheduledBackup) field.ErrorList {
	var allErrs field.ErrorList
	specPath := field.NewPath("spec")

	// Resolve the time zone first (defaulting to UTC, as the reconciler does) so
	// the next-occurrence check below runs in the same location the reconciler
	// uses when it schedules backups.
	loc := time.UTC
	if scheduled.Spec.TimeZone != "" {
		l, err := time.LoadLocation(scheduled.Spec.TimeZone)
		if err != nil {
			allErrs = append(allErrs, field.Invalid(specPath.Child("timeZone"), scheduled.Spec.TimeZone,
				fmt.Sprintf("not a valid IANA time zone: %v", err)))
		} else {
			loc = l
		}
	}

	if sched, err := cron.ParseStandard(scheduled.Spec.Schedule); err != nil {
		allErrs = append(allErrs, field.Invalid(specPath.Child("schedule"), scheduled.Spec.Schedule,
			fmt.Sprintf("not a valid cron expression: %v", err)))
	} else if sched.Next(time.Now().In(loc)).IsZero() {
		// Syntactically valid but unreachable (e.g. "0 0 31 2 *"): cron.Next
		// returns the zero time, which the reconciler cannot schedule.
		allErrs = append(allErrs, field.Invalid(specPath.Child("schedule"), scheduled.Spec.Schedule,
			"cron expression has no upcoming occurrences"))
	}

	return allErrs
}
