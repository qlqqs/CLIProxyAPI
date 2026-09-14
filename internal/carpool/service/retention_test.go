package service

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
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

func TestBillingRetentionUsesCurrentInstantRegardlessOfRetentionDays(t *testing.T) {
	for _, days := range []int64{90, 180, 365} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
			anchor := time.Date(2025, time.January, 15, 9, 0, 0, 0, time.UTC)
			store, err := carpoolsqlite.Open(ctx, carpoolsqlite.Config{Path: filepath.Join(t.TempDir(), "retention.db"), Now: func() time.Time { return now }})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if errClose := store.Close(); errClose != nil {
					t.Error(errClose)
				}
			})
			admin, err := store.BootstrapAdmin(ctx, domain.User{Username: "admin", DefaultDisplayName: "Admin", Role: domain.UserRoleAdmin, Status: domain.UserStatusActive, PasswordHash: "test-hash"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			user, err := store.CreateUser(ctx, domain.User{Username: "passenger", DefaultDisplayName: "Passenger", Role: domain.UserRolePassenger, Status: domain.UserStatusActive, PasswordHash: "test-hash"})
			if err != nil {
				t.Fatal(err)
			}
			car, err := store.CreateCar(ctx, domain.Car{Name: "Car", Status: domain.CarStatusActive})
			if err != nil {
				t.Fatal(err)
			}
			limit := int64(5_000_000_000)
			member, err := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{UserID: user.ID, CarID: car.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID, StartedAt: anchor, BillingAnchorAt: anchor, BillingTimezone: "UTC", MonthlyLimitNanoUSD: &limit}})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = store.SetRetentionOverride(ctx, &days, nil); err != nil {
				t.Fatal(err)
			}
			control, err := NewControl(store, nil, ControlConfig{SessionAbsoluteTTL: time.Hour, SessionIdleTTL: 30 * time.Minute, UsageRetention: 90 * 24 * time.Hour, Now: func() time.Time { return now }, PasswordHasher: PasswordHasher{Params: testPasswordParams()}})
			if err != nil {
				t.Fatal(err)
			}
			for _, start := range []time.Time{now.AddDate(0, -1, 0), now, now.AddDate(0, 1, 0)} {
				_, err = store.EnsureBillingPeriod(ctx, domain.BillingPeriod{MembershipID: member.ID, MemberRefSnapshot: member.MemberRef, CarID: car.ID, Timezone: "UTC", AnchorAt: anchor, From: start, To: start.AddDate(0, 1, 0), LimitNanoUSD: &limit, ConfirmedNanoUSD: 225_000_000, UnknownCostEvents: 3})
				if err != nil {
					t.Fatal(err)
				}
			}
			preview, _, err := control.PreviewRetention(ctx, admin, "reset_current_period")
			if err != nil || preview != 1 {
				t.Fatalf("reset preview=%d err=%v", preview, err)
			}
			job, err := control.RunRetention(ctx, admin, "reset_current_period")
			if err != nil || job.Status != "completed" || job.ExpectedCount != preview || job.DeletedCount != preview {
				t.Fatalf("reset job=%+v err=%v", job, err)
			}
			current, err := store.GetBillingPeriod(ctx, member.ID, now)
			if err != nil {
				t.Fatal(err)
			}
			if current.ConfirmedNanoUSD != 225_000_000 || current.ResetBaselineNanoUSD != current.ConfirmedNanoUSD || current.UnknownCostEvents != 0 {
				t.Fatalf("current totals not reset: %+v", current)
			}
			if current.LimitNanoUSD == nil || *current.LimitNanoUSD != limit || !current.AnchorAt.Equal(anchor) || !current.From.Equal(now) || !current.To.Equal(now.AddDate(0, 1, 0)) {
				t.Fatalf("reset altered period contract: %+v", current)
			}
			unchanged, err := store.CurrentMembership(ctx, user.ID)
			if err != nil || unchanged.MonthlyLimitNanoUSD == nil || *unchanged.MonthlyLimitNanoUSD != limit || !unchanged.BillingAnchorAt.Equal(anchor) || !unchanged.StartedAt.Equal(anchor) {
				t.Fatalf("reset altered membership: %+v err=%v", unchanged, err)
			}
			for _, at := range []time.Time{now.AddDate(0, -1, 0), now.AddDate(0, 1, 0)} {
				period, err := store.GetBillingPeriod(ctx, member.ID, at)
				if err != nil || period.ResetBaselineNanoUSD != 0 || period.UnknownCostEvents != 3 {
					t.Fatalf("reset touched non-current period: %+v err=%v", period, err)
				}
			}
			preview, _, err = control.PreviewRetention(ctx, admin, "closed_periods")
			if err != nil || preview != 1 {
				t.Fatalf("closed-period preview=%d err=%v", preview, err)
			}
			job, err = control.RunRetention(ctx, admin, "closed_periods")
			if err != nil || job.ExpectedCount != preview || job.DeletedCount != preview {
				t.Fatalf("closed-period job=%+v err=%v", job, err)
			}
			if _, err = store.GetBillingPeriod(ctx, member.ID, now.AddDate(0, -1, 0)); !errors.Is(err, domain.ErrNotFound) {
				t.Fatalf("closed period ending at now was not deleted: %v", err)
			}
			for _, at := range []time.Time{now, now.AddDate(0, 1, 0)} {
				if _, err = store.GetBillingPeriod(ctx, member.ID, at); err != nil {
					t.Fatalf("deleted current/future period: %v", err)
				}
			}
		})
	}
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
			if _, _, err = control.PreviewRetention(context.Background(), admin, "usage_details"); err != nil || !repository.cutoff.Equal(want) {
				t.Fatalf("preview cutoff=%s err=%v", repository.cutoff, err)
			}
			if _, err = control.RunRetention(context.Background(), admin, "usage_details"); err != nil || !repository.cutoff.Equal(want) {
				t.Fatalf("execute cutoff=%s err=%v", repository.cutoff, err)
			}
		})
	}
}
