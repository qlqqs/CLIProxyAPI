package carpool

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

func TestRetentionCleanerReloadsSQLitePolicyAndKeepsAuditIndependent(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	store, errOpen := carpoolsqlite.Open(ctx, carpoolsqlite.Config{Path: filepath.Join(t.TempDir(), "policy.db"), Now: func() time.Time { return now }})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() {
		if errClose := store.Close(); errClose != nil {
			t.Error(errClose)
		}
	})
	seedRequest := retentionPolicyRequestSeeder(t, ctx, store)
	cleaner := newRetentionCleaner(store, retentionCleanerConfig{
		UsageRetention: 47 * 24 * time.Hour, AuditRetention: 180 * 24 * time.Hour,
		BatchSize: 2, Now: func() time.Time { return now },
	})
	for index, days := range []int64{365, 180, 90, 0, -1} {
		t.Run(fmt.Sprint(days), func(t *testing.T) {
			var override *int64
			effective := days
			if days >= 0 {
				override = &days
			} else {
				effective = 47
			}
			if _, errSet := store.SetRetentionOverride(ctx, override, nil); errSet != nil {
				t.Fatal(errSet)
			}
			cutoff := now.Add(-time.Duration(effective) * 24 * time.Hour)
			if days == 0 {
				cutoff = time.Date(1960, time.January, 1, 0, 0, 0, 0, time.UTC)
			}
			prefix := fmt.Sprintf("policy-%d-", index)
			for _, sample := range []struct {
				name string
				at   time.Time
			}{
				{"old", cutoff.Add(-time.Microsecond)}, {"boundary", cutoff}, {"recent", cutoff.Add(time.Microsecond)},
			} {
				seedRequest(t, prefix+sample.name, sample.at)
			}
			for _, sample := range []struct {
				name string
				at   time.Time
			}{
				{"old-audit", now.Add(-180*24*time.Hour - time.Microsecond)},
				{"boundary-audit", now.Add(-180 * 24 * time.Hour)},
			} {
				if _, errAudit := store.InsertAuditEvent(ctx, domain.AuditEvent{
					ID: prefix + sample.name, OccurredAt: sample.at, ActorType: "system", ActorRef: "policy-test",
					Action: "retention.fixture", TargetType: "test", TargetRef: prefix, Result: "success",
				}); errAudit != nil {
					t.Fatal(errAudit)
				}
			}
			cleaner.cleanupPass(ctx)
			for _, name := range []string{"old", "boundary", "recent"} {
				detail, errDetail := store.GetUsageRequestDetail(ctx, prefix+name)
				if name == "old" && days != 0 {
					if !errors.Is(errDetail, domain.ErrNotFound) {
						t.Fatalf("expired detail retained: %+v err=%v", detail, errDetail)
					}
				} else if errDetail != nil || len(detail.Events) != 1 {
					t.Fatalf("retained detail/events missing: %+v err=%v", detail, errDetail)
				}
			}
			audits, errAudits := store.ListAuditEvents(ctx, time.Time{}, "", 100)
			if errAudits != nil {
				t.Fatal(errAudits)
			}
			foundBoundary := false
			for _, audit := range audits {
				if audit.ID == prefix+"old-audit" {
					t.Fatal("expired audit retained by usage policy")
				}
				if audit.ID == prefix+"boundary-audit" {
					foundBoundary = true
				}
			}
			if !foundBoundary {
				t.Fatal("audit at independent cutoff was deleted")
			}
		})
	}
}

func retentionPolicyRequestSeeder(t *testing.T, ctx context.Context, store *carpoolsqlite.Store) func(*testing.T, string, time.Time) {
	t.Helper()
	admin, errAdmin := store.BootstrapAdmin(ctx, domain.User{Username: "policy-admin", DefaultDisplayName: "Admin", Role: domain.UserRoleAdmin, Status: domain.UserStatusActive, PasswordHash: "test-hash"}, nil)
	if errAdmin != nil {
		t.Fatal(errAdmin)
	}
	user, errUser := store.CreateUser(ctx, domain.User{Username: "policy-passenger", DefaultDisplayName: "Passenger", Role: domain.UserRolePassenger, Status: domain.UserStatusActive, PasswordHash: "test-hash"})
	if errUser != nil {
		t.Fatal(errUser)
	}
	key, errKey := store.CreateAPIKey(ctx, domain.APIKey{UserID: user.ID, Name: "policy", SecretDigest: []byte("synthetic-digest")})
	if errKey != nil {
		t.Fatal(errKey)
	}
	car, errCar := store.CreateCar(ctx, domain.Car{Name: "Policy car", Status: domain.CarStatusActive})
	if errCar != nil {
		t.Fatal(errCar)
	}
	member, errMember := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{UserID: user.ID, CarID: car.ID, DisplayName: "Passenger", CreatedByUserID: admin.ID}})
	if errMember != nil {
		t.Fatal(errMember)
	}
	assignment, errAssignment := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{CarID: car.ID, AuthID: "synthetic-policy-auth", SafeLabel: "Policy account", ProviderSnapshot: "codex", CreatedByUserID: admin.ID}})
	if errAssignment != nil {
		t.Fatal(errAssignment)
	}
	return func(t *testing.T, id string, at time.Time) {
		t.Helper()
		if _, errBegin := store.BeginProxyRequest(ctx, domain.ProxyRequest{
			RequestID: id, UserID: user.ID, APIKeyID: key.KeyID, CarID: car.ID, MembershipID: member.ID,
			MemberRefSnapshot: member.MemberRef, DisplayNameSnapshot: member.DisplayName,
			ScopeHash: "policy-scope", ScopeSize: 1, SourceFormat: "openai", RequestedModel: "synthetic-model",
			Outcome: domain.RequestOutcomeInProgress, StartedAt: at,
		}, []domain.ProxyRequestAuthScope{{AuthID: assignment.AuthID, AssignmentID: assignment.ID, AccountRefSnapshot: assignment.AccountRef, SafeLabelSnapshot: assignment.SafeLabel, ProviderSnapshot: assignment.ProviderSnapshot}}); errBegin != nil {
			t.Fatal(errBegin)
		}
		if _, errUsage := store.InsertUsageEvent(ctx, domain.UsageEvent{EventID: id + "-event", RequestID: id, AuthID: assignment.AuthID, Provider: "codex", Model: "synthetic-model", RequestedAt: at}); errUsage != nil {
			t.Fatal(errUsage)
		}
		if errComplete := store.CompleteProxyRequest(ctx, domain.RequestCompletion{RequestID: id, Outcome: domain.RequestOutcomeSucceeded, CompletedAt: at, StatusClass: "2xx", UpstreamAttempted: true}); errComplete != nil {
			t.Fatal(errComplete)
		}
	}
}

func TestRetentionCleanerSettingsReadFailureSkipsRemainingBatches(t *testing.T) {
	for _, successfulBatches := range []int{0, 1} {
		t.Run(fmt.Sprint(successfulBatches), func(t *testing.T) {
			steps := make([]retentionPolicyRead, successfulBatches)
			for i := range steps {
				steps[i].settings.EffectiveDays = 90
			}
			steps = append(steps, retentionPolicyRead{err: errors.New("synthetic settings failure")})
			store := &retentionPolicyStoreStub{steps: steps}
			cleaner := newRetentionCleaner(store, retentionCleanerConfig{
				UsageRetention: 90 * 24 * time.Hour, AuditRetention: 180 * 24 * time.Hour, BatchSize: 2,
				Now: func() time.Time { return time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC) },
			})
			cleaner.cleanupPass(context.Background())
			if len(store.cleanups) != successfulBatches {
				t.Fatalf("cleanup calls=%d, want %d; failed settings read must not use fallback", len(store.cleanups), successfulBatches)
			}
			if len(store.defaults) != successfulBatches+1 {
				t.Fatalf("settings reads=%d, want %d", len(store.defaults), successfulBatches+1)
			}
		})
	}
}

func TestRetentionCleanerRechecksPermanentPolicyBetweenBatchesWithFrozenClock(t *testing.T) {
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	store := &retentionPolicyStoreStub{steps: []retentionPolicyRead{
		{settings: domain.RetentionSettings{EffectiveDays: 90}},
		{settings: domain.RetentionSettings{EffectiveDays: 0}},
	}}
	clockCalls := 0
	cleaner := newRetentionCleaner(store, retentionCleanerConfig{
		UsageRetention: 47 * 24 * time.Hour, AuditRetention: 180 * 24 * time.Hour, BatchSize: 2,
		Now: func() time.Time { clockCalls++; return now.Add(time.Duration(clockCalls-1) * time.Hour) },
	})
	cleaner.cleanupPass(context.Background())
	if len(store.cleanups) != 2 || len(store.defaults) != 2 || clockCalls != 1 {
		t.Fatalf("cleanups=%d reads=%d clock calls=%d", len(store.cleanups), len(store.defaults), clockCalls)
	}
	if !store.cleanups[0].UsageCutoff.Equal(now.Add(-90 * 24 * time.Hour)) {
		t.Fatalf("first batch usage cutoff=%s", store.cleanups[0].UsageCutoff)
	}
	if store.cleanups[1].UsageCutoff.UnixMicro() != -1<<63 {
		t.Fatalf("permanent batch cutoff=%s, want minimum stored timestamp", store.cleanups[1].UsageCutoff)
	}
	for i, cleanup := range store.cleanups {
		if !cleanup.AuditCutoff.Equal(now.Add(-180*24*time.Hour)) || store.defaults[i] != 47 {
			t.Fatalf("batch %d changed audit/config cutoff: %+v default=%d", i, cleanup, store.defaults[i])
		}
	}
}

type retentionPolicyRead struct {
	settings domain.RetentionSettings
	err      error
}

type retentionPolicyStoreStub struct {
	steps    []retentionPolicyRead
	defaults []int64
	cleanups []carpoolsqlite.RetentionCleanup
}

func (s *retentionPolicyStoreStub) GetRetentionSettings(_ context.Context, defaultDays int64) (domain.RetentionSettings, error) {
	index := len(s.defaults)
	s.defaults = append(s.defaults, defaultDays)
	if index >= len(s.steps) {
		return domain.RetentionSettings{}, errors.New("unexpected extra settings read")
	}
	return s.steps[index].settings, s.steps[index].err
}

func (s *retentionPolicyStoreStub) CleanupRetention(_ context.Context, cleanup carpoolsqlite.RetentionCleanup) (carpoolsqlite.RetentionCleanupResult, error) {
	s.cleanups = append(s.cleanups, cleanup)
	if len(s.cleanups) < len(s.steps) {
		return carpoolsqlite.RetentionCleanupResult{UsageEventsDeleted: int64(cleanup.BatchSize)}, nil
	}
	return carpoolsqlite.RetentionCleanupResult{}, nil
}
