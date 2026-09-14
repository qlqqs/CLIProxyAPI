package sqlite_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
)

func TestRetentionSettingsPersistenceAndConfiguredResponse(t *testing.T) {
	for _, configDays := range []int64{90, 47, 400} {
		t.Run(fmt.Sprint(configDays), func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
			path := filepath.Join(t.TempDir(), "retention.db")
			store := openRetentionSettingsStore(t, path, now)
			admin := domain.User{Role: domain.UserRoleAdmin, UserRef: "usr_retention_admin"}
			newControl := func(days int64) *service.Control {
				t.Helper()
				control, errControl := service.NewControl(store, nil, service.ControlConfig{
					SessionAbsoluteTTL: time.Hour, SessionIdleTTL: 30 * time.Minute,
					UsageRetention: time.Duration(days) * 24 * time.Hour, Now: func() time.Time { return now },
				})
				if errControl != nil {
					t.Fatal(errControl)
				}
				return control
			}
			assertRead := func(control *service.Control, days int64, override *int64) {
				t.Helper()
				settings, errRead := control.RetentionSettings(ctx, admin)
				if errRead != nil {
					t.Fatal(errRead)
				}
				assertRetentionSettings(t, settings, days, override)
			}
			control := newControl(configDays)
			assertRead(control, configDays, nil)
			for index, value := range []int64{90, 180, 365, 0, -1} {
				var override *int64
				if value >= 0 {
					override = &value
				}
				settings, errUpdate := control.UpdateRetentionSettings(ctx, admin, override)
				if errUpdate != nil {
					t.Fatal(errUpdate)
				}
				assertRetentionSettings(t, settings, configDays, override)
				assertRead(control, configDays, override)
				if errClose := store.Close(); errClose != nil {
					t.Fatal(errClose)
				}
				store = openRetentionSettingsStore(t, path, now)
				control = newControl(configDays)
				assertRead(control, configDays, override)
				assertRead(newControl(configDays+17), configDays+17, override)
				audits, errAudits := store.ListAuditEvents(ctx, time.Time{}, "", 100)
				if errAudits != nil || len(audits) != index+1 {
					t.Fatalf("audits=%+v err=%v", audits, errAudits)
				}
				for _, audit := range audits {
					if audit.Action != "update_retention" || audit.Result != "succeeded" || audit.ActorRef != admin.UserRef || !audit.OccurredAt.Equal(now) {
						t.Fatalf("unexpected settings audit: %+v", audit)
					}
				}
			}
		})
	}
}

func TestRetentionSettingsRejectInvalidAndRollbackAuditFailure(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "retention.db")
	store := openRetentionSettingsStore(t, path, now)
	settings, errRead := store.GetRetentionSettings(ctx, 0)
	if errRead != nil {
		t.Fatal(errRead)
	}
	assertRetentionSettings(t, settings, 90, nil)
	permanent := int64(0)
	if _, errSet := store.SetRetentionOverride(ctx, &permanent, nil); errSet != nil {
		t.Fatal(errSet)
	}
	for _, invalid := range []int64{-1, 30, 47, 91, 366} {
		if _, errSet := store.SetRetentionOverride(ctx, &invalid, nil); !errors.Is(errSet, domain.ErrInvalid) {
			t.Fatalf("SetRetentionOverride(%d) error=%v, want invalid", invalid, errSet)
		}
	}
	if _, errSet := store.SetRetentionOverride(ctx, nil, &domain.AuditEvent{Action: "update_retention"}); !errors.Is(errSet, domain.ErrInvalid) {
		t.Fatalf("invalid audit error=%v", errSet)
	}
	if errClose := store.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	store = openRetentionSettingsStore(t, path, now)
	settings, errRead = store.GetRetentionSettings(ctx, 47)
	if errRead != nil {
		t.Fatal(errRead)
	}
	assertRetentionSettings(t, settings, 47, &permanent)
	audits, errAudits := store.ListAuditEvents(ctx, time.Time{}, "", 100)
	if errAudits != nil || len(audits) != 0 {
		t.Fatalf("audits=%+v err=%v", audits, errAudits)
	}
}

func openRetentionSettingsStore(t *testing.T, path string, now time.Time) *carpoolsqlite.Store {
	t.Helper()
	store, errOpen := carpoolsqlite.Open(context.Background(), carpoolsqlite.Config{Path: path, Now: func() time.Time { return now }})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	return store
}

func assertRetentionSettings(t *testing.T, got domain.RetentionSettings, configDays int64, override *int64) {
	t.Helper()
	effective, source := configDays, "config"
	if override != nil {
		effective, source = *override, "database"
	}
	if got.DefaultDays != configDays || got.EffectiveDays != effective || got.Source != source ||
		(got.OverrideDays == nil) != (override == nil) || (override != nil && *got.OverrideDays != *override) {
		t.Fatalf("settings=%+v, want default=%d effective=%d source=%s override=%v", got, configDays, effective, source, override)
	}
}
