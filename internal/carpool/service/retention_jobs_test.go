package service

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

type confirmedJobRepository struct {
	Repository
	job           domain.RetentionJob
	settingsErr   error
	confirmErr    error
	confirmations int
}

func (r *confirmedJobRepository) GetRetentionSettings(context.Context, int64) (domain.RetentionSettings, error) {
	return domain.RetentionSettings{}, r.settingsErr
}
func (r *confirmedJobRepository) PreviewRetentionJob(context.Context, string, string, time.Time) (domain.RetentionJob, error) {
	return r.job, nil
}
func (r *confirmedJobRepository) ConfirmRetentionJob(context.Context, string, string, string) (domain.RetentionJob, error) {
	r.confirmations++
	return r.job, r.confirmErr
}
func (r *confirmedJobRepository) GetRetentionJob(context.Context, string) (domain.RetentionJob, error) {
	return r.job, nil
}

func TestConfirmedRetentionServiceRequiresAdminBoundJobAndPropagatesErrors(t *testing.T) {
	ctx := context.Background()
	failure := errors.New("synthetic database failure")
	repo := &confirmedJobRepository{job: domain.RetentionJob{ID: "bound-job", ActorRef: "usr_admin"}, settingsErr: failure, confirmErr: failure}
	control, err := NewControl(repo, nil, ControlConfig{SessionAbsoluteTTL: time.Hour, SessionIdleTTL: 30 * time.Minute, UsageRetention: 90 * 24 * time.Hour, PasswordHasher: PasswordHasher{Params: testPasswordParams()}})
	if err != nil {
		t.Fatal(err)
	}
	admin := domain.User{Role: domain.UserRoleAdmin, UserRef: "usr_admin"}
	passenger := domain.User{Role: domain.UserRolePassenger, UserRef: "usr_passenger"}
	if _, err = control.PreviewRetention(ctx, passenger, "usage_details"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("preview role: %v", err)
	}
	if _, err = control.RunRetention(ctx, passenger, "usage_details", "bound-job"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("confirm role: %v", err)
	}
	if _, err = control.RetentionJob(ctx, passenger, "bound-job"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("read role: %v", err)
	}
	if _, err = control.RunRetention(ctx, admin, "usage_details", ""); !errors.Is(err, domain.ErrInvalid) {
		t.Fatalf("missing ID: %v", err)
	}
	if repo.confirmations != 0 {
		t.Fatal("unbound or unauthorized confirm reached store")
	}
	if _, err = control.PreviewRetention(ctx, admin, "usage_details"); !errors.Is(err, failure) {
		t.Fatalf("settings error swallowed: %v", err)
	}
	if _, err = control.PreviewRetention(ctx, admin, "reset_quota_windows"); err != nil {
		t.Fatalf("quota reset depended on detail settings: %v", err)
	}
	if _, err = control.RunRetention(ctx, admin, "reset_quota_windows", "bound-job"); !errors.Is(err, failure) {
		t.Fatalf("transaction error swallowed: %v", err)
	}
	admin.UserRef = "usr_other"
	if _, err = control.RetentionJob(ctx, admin, "bound-job"); !errors.Is(err, ErrForbidden) {
		t.Fatalf("cross-admin result readable: %v", err)
	}
}
