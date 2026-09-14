package service

import (
	"context"
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

	if _, _, errPreview := control.PreviewRetention(context.Background(), admin, "usage_details"); errPreview != nil {
		t.Fatalf("PreviewRetention() error = %v", errPreview)
	}
	if !repository.cutoff.After(now) {
		t.Fatalf("preview cutoff = %s, want just after %s", repository.cutoff, now)
	}

	repository.cutoff = time.Time{}
	if _, errRun := control.RunRetention(context.Background(), admin, "usage_details"); errRun != nil {
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

func (r *manualRetentionRepository) PreviewRetention(_ context.Context, _ string, cutoff time.Time) (int64, int64, error) {
	r.cutoff = cutoff
	return 1, 0, nil
}

func (r *manualRetentionRepository) CreateRetentionJob(_ context.Context, operation, actorRef string, expected int64) (domain.RetentionJob, error) {
	return domain.RetentionJob{ID: "job_1", Operation: operation, ActorRef: actorRef, ExpectedCount: expected}, nil
}

func (r *manualRetentionRepository) ExecuteRetention(_ context.Context, _ string, cutoff time.Time, _ int) (int64, int64, error) {
	r.cutoff = cutoff
	return 1, 0, nil
}

func (r *manualRetentionRepository) FinishRetentionJob(context.Context, string, string, int64, int64, string) error {
	return nil
}
