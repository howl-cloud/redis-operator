package webhooks

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	redisv1 "github.com/howl-cloud/redis-operator/api/v1"
)

func scheduledBackup(schedule, timeZone string) *redisv1.RedisScheduledBackup {
	return &redisv1.RedisScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{Name: "daily", Namespace: "default"},
		Spec: redisv1.RedisScheduledBackupSpec{
			Schedule:    schedule,
			ClusterName: "cluster",
			TimeZone:    timeZone,
		},
	}
}

func TestScheduledBackupValidator_ValidateCreate(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		timeZone string
		wantErr  bool
	}{
		{"hourly", "0 * * * *", "", false},
		{"daily with timezone", "0 2 * * *", "America/Chicago", false},
		{"weekly", "0 3 * * 0", "UTC", false},
		{"descriptor", "@daily", "", false},
		{"invalid cron", "not-a-cron", "", true},
		{"seconds not supported", "0 0 * * * *", "", true},
		{"out of range", "99 0 * * *", "", true},
		{"unreachable date", "0 0 31 2 *", "", true},
		{"invalid timezone", "0 2 * * *", "Mars/Phobos", true},
	}

	v := &RedisScheduledBackupValidator{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := v.ValidateCreate(context.Background(), scheduledBackup(tt.schedule, tt.timeZone))
			if tt.wantErr {
				require.Error(t, err)
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestScheduledBackupValidator_ValidateUpdate(t *testing.T) {
	v := &RedisScheduledBackupValidator{}
	old := scheduledBackup("0 * * * *", "")

	_, err := v.ValidateUpdate(context.Background(), old, scheduledBackup("0 2 * * *", "UTC"))
	require.NoError(t, err)

	_, err = v.ValidateUpdate(context.Background(), old, scheduledBackup("garbage", ""))
	require.Error(t, err)
}

func TestScheduledBackupValidator_WrongType(t *testing.T) {
	v := &RedisScheduledBackupValidator{}
	_, err := v.ValidateCreate(context.Background(), &redisv1.RedisCluster{})
	assert.Error(t, err)
}
