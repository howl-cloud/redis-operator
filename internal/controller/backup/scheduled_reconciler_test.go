package backup

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func int32Ptr(i int32) *int32 {
	return &i
}

func newScheduledReconciler(objs ...client.Object) (*ScheduledBackupReconciler, client.Client) {
	scheme := testScheme()
	builder := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&redisv1.RedisBackup{}, &redisv1.RedisScheduledBackup{})
	if len(objs) > 0 {
		builder = builder.WithObjects(objs...)
	}
	c := builder.Build()
	recorder := record.NewFakeRecorder(100)
	return NewScheduledBackupReconciler(c, scheme, recorder), c
}

func TestParseSchedule_Valid(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		timeZone string
	}{
		{"hourly", "0 * * * *", ""},
		{"daily", "0 2 * * *", "UTC"},
		{"weekly", "0 3 * * 0", "America/Chicago"},
		{"descriptor hourly", "@hourly", ""},
		{"descriptor every", "@every 15m", ""},
		{"empty timezone defaults to utc", "0 0 * * *", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched, loc, err := parseSchedule(tt.schedule, tt.timeZone)
			require.NoError(t, err)
			require.NotNil(t, sched)
			require.NotNil(t, loc)
		})
	}
}

func TestParseSchedule_Invalid(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		timeZone string
	}{
		{"garbage", "not-a-cron", ""},
		{"too few fields", "0 0", ""},
		{"seconds not supported", "0 0 * * * *", ""},
		{"out of range", "99 0 * * *", ""},
		{"unknown timezone", "0 * * * *", "Mars/Phobos"},
		// Note: "0 0 31 2 *" parses successfully but never fires; that zero-time
		// case is rejected by the reconciler and webhook, not by parseSchedule.
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, _, err := parseSchedule(tt.schedule, tt.timeZone)
			require.Error(t, err)
		})
	}
}

func TestNextScheduleTime_Computes(t *testing.T) {
	chicago, err := time.LoadLocation("America/Chicago")
	require.NoError(t, err)

	tests := []struct {
		name     string
		schedule string
		timeZone string
		after    time.Time
		want     time.Time
	}{
		{
			name:     "hourly rolls to next top of hour",
			schedule: "0 * * * *",
			timeZone: "UTC",
			after:    time.Date(2026, 1, 1, 10, 30, 0, 0, time.UTC),
			want:     time.Date(2026, 1, 1, 11, 0, 0, 0, time.UTC),
		},
		{
			name:     "daily 2am rolls to next day",
			schedule: "0 2 * * *",
			timeZone: "UTC",
			after:    time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
			want:     time.Date(2026, 1, 2, 2, 0, 0, 0, time.UTC),
		},
		{
			name:     "weekly sunday 3am",
			schedule: "0 3 * * 0",
			timeZone: "UTC",
			// 2026-01-01 is a Thursday; next Sunday is the 4th.
			after: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
			want:  time.Date(2026, 1, 4, 3, 0, 0, 0, time.UTC),
		},
		{
			name:     "daily honors timezone, not controller clock",
			schedule: "0 2 * * *",
			timeZone: "America/Chicago",
			// 10:00 UTC on Jan 1 is 04:00 CST; next 2am CST is Jan 2 (08:00 UTC).
			after: time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC),
			want:  time.Date(2026, 1, 2, 2, 0, 0, 0, chicago),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sched, loc, err := parseSchedule(tt.schedule, tt.timeZone)
			require.NoError(t, err)
			got := sched.Next(tt.after.In(loc))
			assert.Truef(t, got.Equal(tt.want), "got %s, want %s", got, tt.want)
		})
	}
}

func TestCleanupOldBackups_RemovesExcess(t *testing.T) {
	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily-backup",
			Namespace: "default",
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:                      "0 0 * * *",
			ClusterName:                   "test-cluster",
			SuccessfulBackupsHistoryLimit: int32Ptr(2),
			FailedBackupsHistoryLimit:     int32Ptr(1),
		},
	}

	var objs []client.Object
	objs = append(objs, scheduled)

	// Create 4 successful backups.
	for i := 0; i < 4; i++ {
		objs = append(objs, &redisv1.RedisBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("daily-backup-success-%d", i),
				Namespace: "default",
				Labels: map[string]string{
					"redis.io/scheduled-backup": "daily-backup",
				},
			},
			Status: redisv1.RedisBackupStatus{
				Phase: redisv1.BackupPhaseCompleted,
			},
		})
	}

	// Create 3 failed backups.
	for i := 0; i < 3; i++ {
		objs = append(objs, &redisv1.RedisBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("daily-backup-failed-%d", i),
				Namespace: "default",
				Labels: map[string]string{
					"redis.io/scheduled-backup": "daily-backup",
				},
			},
			Status: redisv1.RedisBackupStatus{
				Phase: redisv1.BackupPhaseFailed,
			},
		})
	}

	r, c := newScheduledReconciler(objs...)
	ctx := context.Background()

	err := r.cleanupOldBackups(ctx, scheduled)
	require.NoError(t, err)

	// Count remaining backups.
	var backupList redisv1.RedisBackupList
	err = c.List(ctx, &backupList, client.InNamespace("default"), client.MatchingLabels{
		"redis.io/scheduled-backup": "daily-backup",
	})
	require.NoError(t, err)

	var successCount, failCount int
	for _, b := range backupList.Items {
		switch b.Status.Phase {
		case redisv1.BackupPhaseCompleted:
			successCount++
		case redisv1.BackupPhaseFailed:
			failCount++
		}
	}

	assert.Equal(t, 2, successCount, "should retain only 2 successful backups")
	assert.Equal(t, 1, failCount, "should retain only 1 failed backup")
}

func TestCleanupOldBackups_DefaultLimits(t *testing.T) {
	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily-backup",
			Namespace: "default",
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 0 * * *",
			ClusterName: "test-cluster",
			// No limits set, defaults to 3 each.
		},
	}

	var objs []client.Object
	objs = append(objs, scheduled)

	// Create 5 successful backups.
	for i := 0; i < 5; i++ {
		objs = append(objs, &redisv1.RedisBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("daily-backup-success-%d", i),
				Namespace: "default",
				Labels: map[string]string{
					"redis.io/scheduled-backup": "daily-backup",
				},
			},
			Status: redisv1.RedisBackupStatus{
				Phase: redisv1.BackupPhaseCompleted,
			},
		})
	}

	r, c := newScheduledReconciler(objs...)
	ctx := context.Background()

	err := r.cleanupOldBackups(ctx, scheduled)
	require.NoError(t, err)

	var backupList redisv1.RedisBackupList
	err = c.List(ctx, &backupList, client.InNamespace("default"), client.MatchingLabels{
		"redis.io/scheduled-backup": "daily-backup",
	})
	require.NoError(t, err)

	var successCount int
	for _, b := range backupList.Items {
		if b.Status.Phase == redisv1.BackupPhaseCompleted {
			successCount++
		}
	}
	assert.Equal(t, 3, successCount, "default limit should retain 3 successful backups")
}

func TestCleanupOldBackups_UnderLimit(t *testing.T) {
	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily-backup",
			Namespace: "default",
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:                      "0 0 * * *",
			ClusterName:                   "test-cluster",
			SuccessfulBackupsHistoryLimit: int32Ptr(5),
		},
	}

	var objs []client.Object
	objs = append(objs, scheduled)

	// Create only 2 successful backups (under limit of 5).
	for i := 0; i < 2; i++ {
		objs = append(objs, &redisv1.RedisBackup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      fmt.Sprintf("daily-backup-success-%d", i),
				Namespace: "default",
				Labels: map[string]string{
					"redis.io/scheduled-backup": "daily-backup",
				},
			},
			Status: redisv1.RedisBackupStatus{
				Phase: redisv1.BackupPhaseCompleted,
			},
		})
	}

	r, c := newScheduledReconciler(objs...)
	ctx := context.Background()

	err := r.cleanupOldBackups(ctx, scheduled)
	require.NoError(t, err)

	var backupList redisv1.RedisBackupList
	err = c.List(ctx, &backupList, client.InNamespace("default"), client.MatchingLabels{
		"redis.io/scheduled-backup": "daily-backup",
	})
	require.NoError(t, err)

	var successCount int
	for _, b := range backupList.Items {
		if b.Status.Phase == redisv1.BackupPhaseCompleted {
			successCount++
		}
	}
	assert.Equal(t, 2, successCount, "should not delete when under limit")
}

func boolPtr(b bool) *bool {
	return &b
}

func TestScheduledReconcile_NotFound(t *testing.T) {
	r, _ := newScheduledReconciler()
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "nonexistent", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)
}

func TestScheduledReconcile_Suspended(t *testing.T) {
	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily",
			Namespace: "default",
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 0 * * *",
			ClusterName: "cluster",
			Suspend:     boolPtr(true),
		},
	}

	r, c := newScheduledReconciler(scheduled)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)

	// Verify status is Suspended.
	var updated redisv1.RedisScheduledBackup
	err = c.Get(ctx, types.NamespacedName{Name: "daily", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.ScheduledBackupPhaseSuspended, updated.Status.Phase)
}

func TestScheduledReconcile_ClusterNotFound(t *testing.T) {
	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily",
			Namespace: "default",
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 0 * * *",
			ClusterName: "missing-cluster",
		},
	}

	r, _ := newScheduledReconciler(scheduled)
	ctx := context.Background()

	_, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	require.Error(t, err)
	assert.Contains(t, err.Error(), "cluster missing-cluster not found")
}

func TestScheduledReconcile_NotYetDue(t *testing.T) {
	// Just ran; the next daily occurrence is in the future, so nothing fires.
	lastSchedule := metav1.NewTime(time.Now())

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
	}

	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily",
			Namespace: "default",
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 2 * * *",
			ClusterName: "cluster",
		},
		Status: redisv1.RedisScheduledBackupStatus{
			LastScheduleTime: &lastSchedule,
		},
	}

	r, c := newScheduledReconciler(cluster, scheduled)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	require.NoError(t, err)
	// Should requeue for the next occurrence, at most ~24h out.
	assert.True(t, result.RequeueAfter > 0, "should requeue with a delay")
	assert.True(t, result.RequeueAfter <= 24*time.Hour, "requeue should be within ~24h")

	// No backup should have been created.
	var backupList redisv1.RedisBackupList
	require.NoError(t, c.List(ctx, &backupList, client.InNamespace("default")))
	assert.Empty(t, backupList.Items)

	var updated redisv1.RedisScheduledBackup
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "daily", Namespace: "default"}, &updated))
	assert.Equal(t, redisv1.ScheduledBackupPhaseActive, updated.Status.Phase)
	require.NotNil(t, updated.Status.NextScheduleTime)
	assert.True(t, updated.Status.NextScheduleTime.After(time.Now()), "next schedule time should be in the future")
	cond := meta.FindStatusCondition(updated.Status.Conditions, redisv1.ScheduledBackupConditionScheduleValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionTrue, cond.Status)
}

func TestScheduledReconcile_CreateBackup(t *testing.T) {
	// Last schedule was 2h ago on an hourly schedule, so an occurrence is due.
	lastSchedule := metav1.NewTime(time.Now().Add(-2 * time.Hour))

	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
	}

	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily",
			Namespace: "default",
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 * * * *",
			ClusterName: "cluster",
			Target:      redisv1.BackupTargetPrimary,
		},
		Status: redisv1.RedisScheduledBackupStatus{
			LastScheduleTime: &lastSchedule,
		},
	}

	r, c := newScheduledReconciler(cluster, scheduled)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	require.NoError(t, err)
	// Requeue is now driven by the next cron occurrence (within the next hour).
	assert.True(t, result.RequeueAfter > 0, "should requeue for the next occurrence")
	assert.True(t, result.RequeueAfter <= time.Hour, "next hourly occurrence is within an hour")

	// Verify a RedisBackup was created.
	var backupList redisv1.RedisBackupList
	err = c.List(ctx, &backupList, client.InNamespace("default"))
	require.NoError(t, err)
	require.Len(t, backupList.Items, 1)

	backup := backupList.Items[0]
	assert.Equal(t, "cluster", backup.Spec.ClusterName)
	assert.Equal(t, redisv1.BackupTargetPrimary, backup.Spec.Target)
	assert.Contains(t, backup.Labels, "redis.io/scheduled-backup")
	assert.Equal(t, "daily", backup.Labels["redis.io/scheduled-backup"])

	// Verify status was updated with last/next schedule time and backup name.
	var updated redisv1.RedisScheduledBackup
	err = c.Get(ctx, types.NamespacedName{Name: "daily", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.NotNil(t, updated.Status.LastScheduleTime)
	assert.NotEmpty(t, updated.Status.LastBackupName)
	require.NotNil(t, updated.Status.NextScheduleTime)
	assert.True(t, updated.Status.NextScheduleTime.After(time.Now()), "next schedule time should be in the future")
}

func TestScheduledReconcile_FirstRunWaitsForNextTick(t *testing.T) {
	// Freshly created (creationTimestamp ~now): the first backup should wait for
	// the next cron occurrence rather than firing immediately.
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
	}

	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "daily",
			Namespace:         "default",
			CreationTimestamp: metav1.Now(),
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 2 * * *",
			ClusterName: "cluster",
		},
	}

	r, c := newScheduledReconciler(cluster, scheduled)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	// No backup yet.
	var backupList redisv1.RedisBackupList
	require.NoError(t, c.List(ctx, &backupList, client.InNamespace("default")))
	assert.Empty(t, backupList.Items)
}

func TestScheduledReconcile_CatchUpAfterDowntime(t *testing.T) {
	// Created two days ago, never run (controller was down). At-most-one catch-up:
	// exactly one backup fires, regardless of how many windows were missed.
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
	}

	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:              "daily",
			Namespace:         "default",
			CreationTimestamp: metav1.NewTime(time.Now().Add(-48 * time.Hour)),
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 2 * * *",
			ClusterName: "cluster",
		},
	}

	r, c := newScheduledReconciler(cluster, scheduled)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.True(t, result.RequeueAfter > 0)

	var backupList redisv1.RedisBackupList
	require.NoError(t, c.List(ctx, &backupList, client.InNamespace("default")))
	assert.Len(t, backupList.Items, 1, "exactly one catch-up backup, not one per missed window")

	var updated redisv1.RedisScheduledBackup
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "daily", Namespace: "default"}, &updated))
	require.NotNil(t, updated.Status.NextScheduleTime)
	assert.True(t, updated.Status.NextScheduleTime.After(time.Now()))
}

func TestScheduledReconcile_InvalidSchedule(t *testing.T) {
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
	}

	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily",
			Namespace: "default",
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "not-a-cron",
			ClusterName: "cluster",
		},
	}

	r, c := newScheduledReconciler(cluster, scheduled)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	// No error and no requeue: a static bad value won't fix itself by retrying.
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)

	// No backup created.
	var backupList redisv1.RedisBackupList
	require.NoError(t, c.List(ctx, &backupList, client.InNamespace("default")))
	assert.Empty(t, backupList.Items)

	// ScheduleValid=False condition surfaced.
	var updated redisv1.RedisScheduledBackup
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "daily", Namespace: "default"}, &updated))
	cond := meta.FindStatusCondition(updated.Status.Conditions, redisv1.ScheduledBackupConditionScheduleValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "InvalidSchedule", cond.Reason)
}

func TestScheduledReconcile_UnreachableSchedule(t *testing.T) {
	// "0 0 31 2 *" parses but never fires (February 31st). robfig/cron returns
	// the zero time, which must not be treated as "due".
	cluster := &redisv1.RedisCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster",
			Namespace: "default",
		},
	}

	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily",
			Namespace: "default",
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    "0 0 31 2 *",
			ClusterName: "cluster",
		},
	}

	r, c := newScheduledReconciler(cluster, scheduled)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	// No error, no requeue, and crucially no backup — otherwise this would loop
	// every second forever.
	require.NoError(t, err)
	assert.Equal(t, reconcile.Result{}, result)

	var backupList redisv1.RedisBackupList
	require.NoError(t, c.List(ctx, &backupList, client.InNamespace("default")))
	assert.Empty(t, backupList.Items)

	var updated redisv1.RedisScheduledBackup
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "daily", Namespace: "default"}, &updated))
	assert.Nil(t, updated.Status.NextScheduleTime, "must not record a zero next schedule time")
	cond := meta.FindStatusCondition(updated.Status.Conditions, redisv1.ScheduledBackupConditionScheduleValid)
	require.NotNil(t, cond)
	assert.Equal(t, metav1.ConditionFalse, cond.Status)
	assert.Equal(t, "InvalidSchedule", cond.Reason)
}
