package backup

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestNextScheduleTime_NeverRun(t *testing.T) {
	now := time.Now()
	next, err := nextScheduleTime("0 * * * *", nil, now)
	require.NoError(t, err)
	assert.Equal(t, now, next, "should return now for never-run schedule")
}

func TestNextScheduleTime_Overdue(t *testing.T) {
	now := time.Now()
	lastSchedule := metav1.NewTime(now.Add(-2 * time.Hour))

	next, err := nextScheduleTime("0 * * * *", &lastSchedule, now)
	require.NoError(t, err)
	// Last was 2h ago, interval is 1h, so next would be 1h ago = overdue = now.
	assert.Equal(t, now, next)
}

func TestNextScheduleTime_NotYetDue(t *testing.T) {
	now := time.Now()
	lastSchedule := metav1.NewTime(now.Add(-30 * time.Minute))

	next, err := nextScheduleTime("0 * * * *", &lastSchedule, now)
	require.NoError(t, err)
	// Last was 30min ago, interval is 1h, so next is 30min from now.
	expected := lastSchedule.Add(1 * time.Hour)
	assert.Equal(t, expected, next)
	assert.True(t, next.After(now))
}

func TestCleanupOldBackups_RemovesExcess(t *testing.T) {
	scheduled := &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "daily-backup",
			Namespace: "default",
		},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:                     "0 0 * * *",
			ClusterName:                  "test-cluster",
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
			Schedule:                     "0 0 * * *",
			ClusterName:                  "test-cluster",
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
	// Last schedule was 10 minutes ago, interval is 1 hour, so next is in 50 min.
	lastSchedule := metav1.NewTime(time.Now().Add(-10 * time.Minute))

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
	// Should requeue with a delay (approximately 50 minutes).
	assert.True(t, result.RequeueAfter > 0, "should requeue with a delay")
	assert.True(t, result.RequeueAfter <= 51*time.Minute, "requeue should be within ~50 min")

	// Verify status phase is Active.
	var updated redisv1.RedisScheduledBackup
	err = c.Get(ctx, types.NamespacedName{Name: "daily", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.Equal(t, redisv1.ScheduledBackupPhaseActive, updated.Status.Phase)
}

func TestScheduledReconcile_CreateBackup(t *testing.T) {
	// Last schedule was 2 hours ago, interval is 1 hour, so it's overdue.
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
	assert.Equal(t, requeueInterval, result.RequeueAfter)

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

	// Verify status was updated with last schedule time and backup name.
	var updated redisv1.RedisScheduledBackup
	err = c.Get(ctx, types.NamespacedName{Name: "daily", Namespace: "default"}, &updated)
	require.NoError(t, err)
	assert.NotNil(t, updated.Status.LastScheduleTime)
	assert.NotEmpty(t, updated.Status.LastBackupName)
}

func TestScheduledReconcile_FirstRun(t *testing.T) {
	// No last schedule time -- should run immediately.
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
		},
	}

	r, c := newScheduledReconciler(cluster, scheduled)
	ctx := context.Background()

	result, err := r.Reconcile(ctx, reconcile.Request{
		NamespacedName: types.NamespacedName{Name: "daily", Namespace: "default"},
	})
	require.NoError(t, err)
	assert.Equal(t, requeueInterval, result.RequeueAfter)

	// Verify a RedisBackup was created.
	var backupList redisv1.RedisBackupList
	err = c.List(ctx, &backupList, client.InNamespace("default"))
	require.NoError(t, err)
	require.Len(t, backupList.Items, 1)
}
