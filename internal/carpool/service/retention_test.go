package service

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestManualRetentionWithPermanentPolicyUsesCurrentCutoff(t *testing.T) {
	now := time.Date(2026, time.September, 5, 17, 0, 0, 0, time.UTC)
	repository := &manualRetentionRepository{
		settings: domain.RetentionSettings{DefaultDays: 90, EffectiveDays: 0, Source: "database"},
	}
	control, errControl := NewControl(repository, nil, ControlConfig{
		SessionAbsoluteTTL: time.Hour,
		SessionIdleTTL:     30 * time.Minute,
		UsageRetention:     90 * 24 * time.Hour,
		Now:                func() time.Time { return now },
	})
	if errControl != nil {
		t.Fatalf("NewControl() error = %v", errControl)
	}
	admin := domain.User{Role: domain.UserRoleAdmin, UserRef: "usr_admin"}

	if _, errPreview := control.PreviewRetention(context.Background(), admin, "usage_details"); errPreview != nil {
		t.Fatalf("PreviewRetention() error = %v", errPreview)
	}
	if !repository.cutoff.After(now) {
		t.Fatalf("preview cutoff = %s, want just after %s", repository.cutoff, now)
	}

	if _, errRun := control.RunRetention(context.Background(), admin, "usage_details", "job_1"); errRun != nil {
		t.Fatalf("RunRetention() error = %v", errRun)
	}
	if !repository.cutoff.After(now) {
		t.Fatalf("run cutoff = %s, want just after %s", repository.cutoff, now)
	}
}

type manualRetentionRepository struct {
	Repository
	settings domain.RetentionSettings
	cutoff   time.Time
}

func (r *manualRetentionRepository) GetRetentionSettings(context.Context, int64) (domain.RetentionSettings, error) {
	return r.settings, nil
}

func (r *manualRetentionRepository) PreviewRetentionJob(_ context.Context, operation, actorRef string, cutoff time.Time) (domain.RetentionJob, error) {
	r.cutoff = cutoff
	return domain.RetentionJob{ID: "job_1", Operation: operation, ActorRef: actorRef, ExpectedCount: 1}, nil
}

func (r *manualRetentionRepository) ConfirmRetentionJob(_ context.Context, id, operation, actorRef string) (domain.RetentionJob, error) {
	return domain.RetentionJob{ID: id, Operation: operation, ActorRef: actorRef, ExpectedCount: 1, DeletedCount: 1, Status: "completed"}, nil
}

func TestUsageDetailsRetainsConfiguredAgeCutoff(t *testing.T) {
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	for _, days := range []int64{90, 180, 365} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			repository := &manualRetentionRepository{settings: domain.RetentionSettings{EffectiveDays: days}}
			control, err := NewControl(repository, nil, ControlConfig{SessionAbsoluteTTL: time.Hour, SessionIdleTTL: 30 * time.Minute, UsageRetention: 90 * 24 * time.Hour, Now: func() time.Time { return now }, PasswordHasher: PasswordHasher{Params: testPasswordParams()}})
			if err != nil {
				t.Fatal(err)
			}
			admin := domain.User{Role: domain.UserRoleAdmin, UserRef: "admin"}
			want := now.Add(-time.Duration(days) * 24 * time.Hour)
			if _, err = control.PreviewRetention(context.Background(), admin, "usage_details"); err != nil || !repository.cutoff.Equal(want) {
				t.Fatalf("preview cutoff=%s err=%v", repository.cutoff, err)
			}
			if _, err = control.RunRetention(context.Background(), admin, "usage_details", "job_1"); err != nil || !repository.cutoff.Equal(want) {
				t.Fatalf("execute cutoff=%s err=%v", repository.cutoff, err)
			}
		})
	}
}
