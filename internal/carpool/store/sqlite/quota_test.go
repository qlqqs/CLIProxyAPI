package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func quotaTestFixture(t *testing.T, now *time.Time) (*Store, retentionFixture) {
	t.Helper()
	store := openTestStore(t, filepath.Join(t.TempDir(), "quota.db"), func() time.Time { return *now })
	t.Cleanup(func() { closeTestStore(t, store) })
	f := newRetentionFixture(t, context.Background(), store)
	monthly := int64(math.MaxInt64)
	if _, err := store.SetMonthlyLimit(context.Background(), f.membership.ID, &monthly, nil); err != nil {
		t.Fatal(err)
	}
	return store, f
}

func quotaAuthorize(t *testing.T, s *Store, f retentionFixture, id string, at time.Time) domain.AuthorizationSnapshot {
	t.Helper()
	snapshot, err := s.AuthorizeAndBeginProxyRequest(context.Background(), quotaAuthorization(f, id, at))
	if err != nil {
		t.Fatalf("authorize %s: %v (%s)", id, err, snapshot.Request.ReasonCode)
	}
	return snapshot
}

func quotaAuthorization(f retentionFixture, id string, at time.Time) domain.ProxyAuthorization {
	return domain.ProxyAuthorization{RequestID: id, UserID: f.user.ID, APIKeyID: f.apiKey.KeyID, SourceFormat: "openai", StartedAt: at, RuntimeAuthIDs: []string{f.assignment.AuthID}}
}

func quotaCharge(t *testing.T, s *Store, f retentionFixture, requestID, eventID string, cost *int64) domain.UsageEvent {
	t.Helper()
	event := domain.UsageEvent{EventID: eventID, RequestID: requestID, AuthID: f.assignment.AuthID, Model: "test", UsageKnown: cost != nil}
	result, err := s.RecordUsageEventBilled(context.Background(), event, "", cost, "", "")
	if err != nil {
		t.Fatalf("charge %s: %v", eventID, err)
	}
	return result
}

func quotaObserve(t *testing.T, s *Store, f retentionFixture, kind string, from, reset, observed time.Time) {
	t.Helper()
	err := s.ObserveQuotaWindows(context.Background(), []domain.QuotaWindowObservation{{AuthID: f.assignment.AuthID, Kind: kind, From: from, ResetAt: reset, ObservedAt: observed}})
	if err != nil {
		t.Fatal(err)
	}
}

func quotaView(t *testing.T, s *Store, f retentionFixture, at time.Time) []domain.MemberQuotaWindow {
	t.Helper()
	result, err := s.MemberQuotaWindows(context.Background(), f.membership.ID, f.assignment.AuthID, at)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func TestMemberQuotaLimitsPartialValidationAndAtomicAudit(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s, f := quotaTestFixture(t, &now)
	initial, err := s.GetMemberQuotaLimits(ctx, f.membership.ID)
	if err != nil || initial.FiveHourNanoUSD != nil || initial.WeeklyNanoUSD != nil || initial.UserConcurrency != nil {
		t.Fatalf("initial limits: %+v %v", initial, err)
	}
	five, week, month := int64(0), int64(200), int64(300)
	concurrency := 2
	update := domain.MemberLimitsUpdate{FiveHourSet: true, WeeklySet: true, UserConcurrencySet: true, FiveHourNanoUSD: &five, WeeklyNanoUSD: &week, UserConcurrency: &concurrency}
	if err = s.SetMemberLimits(ctx, f.membership.ID, update, nil); err != nil {
		t.Fatal(err)
	}
	got, err := s.GetMemberQuotaLimits(ctx, f.membership.ID)
	if err != nil || *got.FiveHourNanoUSD != 0 || *got.WeeklyNanoUSD != 200 || *got.MonthlyNanoUSD != *initial.MonthlyNanoUSD || *got.UserConcurrency != 2 {
		t.Fatalf("limits: %+v %v", got, err)
	}
	snapshot := quotaAuthorize(t, s, f, "month", now)
	if err = s.SetMemberLimits(ctx, f.membership.ID, domain.MemberLimitsUpdate{MonthlySet: true, MonthlyNanoUSD: &month}, nil); err != nil {
		t.Fatal(err)
	}
	period, err := s.GetBillingPeriod(ctx, f.membership.ID, now)
	if err != nil || period.ID != snapshot.Request.BillingPeriodID || *period.LimitNanoUSD != 300 {
		t.Fatalf("month: %+v %v", period, err)
	}
	if err = s.SetMemberLimits(ctx, f.membership.ID, domain.MemberLimitsUpdate{FiveHourSet: true, UserConcurrencySet: true}, nil); err != nil {
		t.Fatal(err)
	}
	got, err = s.GetMemberQuotaLimits(ctx, f.membership.ID)
	if err != nil || got.FiveHourNanoUSD != nil || got.UserConcurrency != nil || *got.WeeklyNanoUSD != week || *got.MonthlyNanoUSD != month {
		t.Fatalf("clear: %+v %v", got, err)
	}
	negative := int64(-1)
	zero, negativeInt := 0, -1
	for _, update := range []domain.MemberLimitsUpdate{{}, {MonthlySet: true, MonthlyNanoUSD: &negative}, {FiveHourSet: true, FiveHourNanoUSD: &negative}, {WeeklySet: true, WeeklyNanoUSD: &negative}, {UserConcurrencySet: true, UserConcurrency: &zero}, {UserConcurrencySet: true, UserConcurrency: &negativeInt}} {
		if err = s.SetMemberLimits(ctx, f.membership.ID, update, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid update %+v: %v", update, err)
		}
	}
	if _, err = s.db.ExecContext(ctx, `CREATE TRIGGER fail_quota_audit BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'audit failure'); END`); err != nil {
		t.Fatal(err)
	}
	audit := &domain.AuditEvent{ActorType: "admin", ActorRef: "test", Action: "limits", TargetType: "member", TargetRef: "test", Result: "succeeded"}
	if err = s.SetMemberLimits(ctx, f.membership.ID, domain.MemberLimitsUpdate{MonthlySet: true, MonthlyNanoUSD: &five, WeeklySet: true, WeeklyNanoUSD: &five, UserConcurrencySet: true, UserConcurrency: &concurrency}, audit); err == nil {
		t.Fatal("audit failure committed")
	}
	got, err = s.GetMemberQuotaLimits(ctx, f.membership.ID)
	if err != nil || *got.MonthlyNanoUSD != month || *got.WeeklyNanoUSD != week || got.UserConcurrency != nil {
		t.Fatalf("audit rollback %+v %v", got, err)
	}
	if err = s.SetAccountConcurrency(ctx, f.assignment.AuthID, &concurrency, audit); err == nil {
		t.Fatal("account audit failure committed")
	}
	account, err := s.GetAccountConcurrency(ctx, f.assignment.AuthID)
	if err != nil || account != nil {
		t.Fatalf("account rollback %v %v", account, err)
	}
}

func TestQuotaPendingRetrospectiveFeesRetentionAndReplay(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	enabled := now
	s, f := quotaTestFixture(t, &now)
	zero := int64(0)
	if err := s.SetMemberLimits(ctx, f.membership.ID, domain.MemberLimitsUpdate{FiveHourSet: true, WeeklySet: true, FiveHourNanoUSD: &zero, WeeklyNanoUSD: &zero}, nil); err != nil {
		t.Fatal(err)
	}
	snapshot := quotaAuthorize(t, s, f, "first-signal", now)
	cost := int64(125)
	event := quotaCharge(t, s, f, snapshot.Request.RequestID, "known", &cost)
	quotaCharge(t, s, f, snapshot.Request.RequestID, "unknown", nil)
	pending := quotaView(t, s, f, now)
	if !pending[0].PendingSync || !pending[1].PendingSync || !pending[0].ResetAt.IsZero() {
		t.Fatalf("pending: %+v", pending)
	}
	if err := s.CompleteProxyRequest(ctx, domain.RequestCompletion{RequestID: snapshot.Request.RequestID, Outcome: domain.RequestOutcomeSucceeded, CompletedAt: now}); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	deleted, err := s.CleanupRetention(ctx, RetentionCleanup{UsageCutoff: now, AuditCutoff: now, BatchSize: 100})
	if err != nil || deleted.ProxyRequestsDeleted != 1 || deleted.UsageEventsDeleted != 2 {
		t.Fatalf("cleanup %+v %v", deleted, err)
	}
	// No detail rows remain when the first window arrives.
	quotaObserve(t, s, f, "5h", enabled.Add(-time.Hour), enabled.Add(4*time.Hour), now)
	quotaObserve(t, s, f, "7d", enabled.Add(-24*time.Hour), enabled.Add(6*24*time.Hour), now)
	views := quotaView(t, s, f, now)
	for _, view := range views {
		if view.PendingSync || view.ConfirmedNanoUSD != cost || view.UnknownCostEvents != 1 || !view.CoverageFrom.Equal(enabled) {
			t.Fatalf("reattributed: %+v", view)
		}
	}
	replayCost := int64(999)
	replay, err := s.RecordUsageEventBilled(ctx, event, "", &replayCost, "priced", "")
	if err != nil || replay.CostNanoUSD == nil || *replay.CostNanoUSD != cost {
		t.Fatalf("retained receipt replay: %+v %v", replay, err)
	}
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM usage_events", 0)
	for _, view := range quotaView(t, s, f, now) {
		if view.ConfirmedNanoUSD != cost || view.UnknownCostEvents != 1 {
			t.Fatalf("double replay: %+v", view)
		}
	}
	// A normal monthly reset never edits the short ledger.
	job, err := s.PreviewRetentionJob(ctx, "reset_current_period", "test-admin", now)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.ConfirmRetentionJob(ctx, job.ID, job.Operation, "test-admin"); err != nil {
		t.Fatal(err)
	}
	period, err := s.GetBillingPeriod(ctx, f.membership.ID, now)
	if err != nil || period.ConfirmedNanoUSD-period.ResetBaselineNanoUSD != 0 || period.UnknownCostEvents != 0 {
		t.Fatalf("monthly reset: %+v %v", period, err)
	}
	rejected, err := s.AuthorizeAndBeginProxyRequest(ctx, quotaAuthorization(f, "exhausted", now))
	if !errors.Is(err, domain.ErrAuthorizationRejected) || rejected.Request.ReasonCode != "five_hour_quota_exhausted" {
		t.Fatalf("short gate: %+v %v", rejected.Request, err)
	}
	if err = s.SetMemberLimits(ctx, f.membership.ID, domain.MemberLimitsUpdate{FiveHourSet: true}, nil); err != nil {
		t.Fatal(err)
	}
	rejected, err = s.AuthorizeAndBeginProxyRequest(ctx, quotaAuthorization(f, "weekly-exhausted", now))
	if !errors.Is(err, domain.ErrAuthorizationRejected) || rejected.Request.ReasonCode != "weekly_quota_exhausted" {
		t.Fatalf("weekly gate: %+v %v", rejected.Request, err)
	}
	input := quotaAuthorization(f, "models", now)
	input.NonBillable = true
	if _, err = s.AuthorizeAndBeginProxyRequest(ctx, input); err != nil {
		t.Fatalf("nonbillable short gate: %v", err)
	}
	now = enabled.Add(8 * 24 * time.Hour)
	expired := quotaView(t, s, f, now)
	if !expired[0].PendingSync || !expired[1].PendingSync {
		t.Fatalf("expired: %+v", expired)
	}
	quotaAuthorize(t, s, f, "expired-may-sync", now)
}

func TestQuotaCorrectionsDuplicatesStaleAndLongRequest(t *testing.T) {
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	start := now
	s, f := quotaTestFixture(t, &now)
	reset := start.Add(4 * time.Hour)
	quotaObserve(t, s, f, "5h", start.Add(-time.Hour), reset, now)
	quotaObserve(t, s, f, "7d", start.Add(-time.Hour), start.Add(167*time.Hour), now)
	quotaAuthorize(t, s, f, "long", now)
	cost := int64(50)
	quotaCharge(t, s, f, "long", "early", &cost)
	now = now.Add(time.Minute)
	quotaObserve(t, s, f, "5h", start.Add(-time.Hour+30*time.Second), reset.Add(30*time.Second), now)
	quotaObserve(t, s, f, "5h", start.Add(-time.Hour), reset, start)                                  // Stale.
	quotaObserve(t, s, f, "5h", start.Add(-time.Hour+30*time.Second), reset.Add(30*time.Second), now) // Duplicate.
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM quota_windows WHERE kind='5h'", 1)
	view := quotaView(t, s, f, now)[0]
	if view.ConfirmedNanoUSD != 50 || !view.ResetAt.Equal(reset.Add(30*time.Second)) || !view.From.Equal(start.Add(-time.Hour+30*time.Second)) {
		t.Fatalf("correction lost fees: %+v", view)
	}
	now = reset.Add(31 * time.Second)
	quotaObserve(t, s, f, "5h", reset.Add(30*time.Second), reset.Add(5*time.Hour+30*time.Second), now)
	quotaCharge(t, s, f, "long", "late", &cost)
	views := quotaView(t, s, f, now)
	if views[0].ConfirmedNanoUSD != 0 || views[1].ConfirmedNanoUSD != 100 {
		t.Fatalf("completion-time misattribution: %+v", views)
	}
	previous := quotaView(t, s, f, start)[0]
	if previous.ConfirmedNanoUSD != 100 {
		t.Fatalf("old window: %+v", previous)
	}
	quotaAuthorize(t, s, f, "new", now)
	quotaCharge(t, s, f, "new", "new-fee", &cost)
	if view = quotaView(t, s, f, now)[0]; view.ConfirmedNanoUSD != 50 {
		t.Fatalf("new window: %+v", view)
	}
	// An observed earlier window arriving after the new one cannot move it backwards.
	quotaObserve(t, s, f, "5h", start.Add(-time.Hour), reset, start.Add(time.Hour))
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM quota_windows WHERE kind='5h'", 2)
	if view = quotaView(t, s, f, now)[0]; view.ConfirmedNanoUSD != 50 {
		t.Fatalf("stale: %+v", view)
	}
}

func TestQuotaCheckOnlyAndAuthorizationFences(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s, f := quotaTestFixture(t, &now)
	input := quotaAuthorization(f, "check", now)
	input.CheckOnly = true
	snapshot, err := s.AuthorizeAndBeginProxyRequest(ctx, input)
	if err != nil || len(snapshot.Scopes) != 1 {
		t.Fatalf("check: %+v %v", snapshot, err)
	}
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM proxy_requests", 0)
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM billing_periods", 0)
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM quota_request_fees", 0)
	for _, test := range []struct{ member, auth, reason string }{{"different", f.assignment.AuthID, "membership_changed"}, {f.membership.ID, "different", "account_changed"}} {
		input.ExpectedMembershipID, input.ExpectedAuthID = test.member, test.auth
		snapshot, err = s.AuthorizeAndBeginProxyRequest(ctx, input)
		if !errors.Is(err, domain.ErrAuthorizationRejected) || snapshot.Request.ReasonCode != test.reason {
			t.Fatalf("fence %+v: %+v %v", test, snapshot.Request, err)
		}
	}
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM audit_events", 0)
	quotaObserve(t, s, f, "5h", now.Add(-time.Hour), now.Add(4*time.Hour), now)
	zero := int64(0)
	if err = s.SetMemberLimits(ctx, f.membership.ID, domain.MemberLimitsUpdate{FiveHourSet: true, FiveHourNanoUSD: &zero}, nil); err != nil {
		t.Fatal(err)
	}
	input.ExpectedMembershipID, input.ExpectedAuthID = "", ""
	snapshot, err = s.AuthorizeAndBeginProxyRequest(ctx, input)
	if !errors.Is(err, domain.ErrAuthorizationRejected) || snapshot.Request.ReasonCode != "five_hour_quota_exhausted" {
		t.Fatalf("preliminary quota: %+v %v", snapshot.Request, err)
	}
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM proxy_requests", 0)
	input.CheckOnly = false
	snapshot, err = s.AuthorizeAndBeginProxyRequest(ctx, input)
	if !errors.Is(err, domain.ErrAuthorizationRejected) || snapshot.Request.ReasonCode != "five_hour_quota_exhausted" {
		t.Fatalf("final quota: %+v %v", snapshot.Request, err)
	}
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM proxy_requests WHERE outcome='rejected'", 1)
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM audit_events", 1)
}

func TestQuotaDuplicateTransactionsAndOverflowRollback(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s, f := quotaTestFixture(t, &now)
	quotaObserve(t, s, f, "5h", now.Add(-time.Hour), now.Add(4*time.Hour), now)
	quotaAuthorize(t, s, f, "duplicate", now)
	cost := int64(1)
	event := domain.UsageEvent{EventID: "shared", RequestID: "duplicate", AuthID: f.assignment.AuthID, Model: "test", UsageKnown: true}
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for range 8 {
		wg.Go(func() { _, err := s.RecordUsageEventBilled(ctx, event, "", &cost, "", ""); errs <- err })
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	if view := quotaView(t, s, f, now)[0]; view.ConfirmedNanoUSD != 1 {
		t.Fatalf("duplicates: %+v", view)
	}
	// Force an accumulated near-overflow value without billions of fixture events.
	if _, err := s.db.ExecContext(ctx, `UPDATE quota_request_fees SET confirmed_nano_usd=?`, int64(math.MaxInt64)); err != nil {
		t.Fatal(err)
	}
	event.EventID = "overflow"
	if _, err := s.RecordUsageEventBilled(ctx, event, "", &cost, "", ""); err == nil {
		t.Fatal("overflow accepted")
	}
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM billed_event_receipts WHERE event_id='overflow'", 0)
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM usage_events WHERE event_id='overflow'", 0)
	period, err := s.GetBillingPeriod(ctx, f.membership.ID, now)
	if err != nil || period.ConfirmedNanoUSD != 1 {
		t.Fatalf("overflow month rollback: %+v %v", period, err)
	}
	// Distinct request rows must also be checked as an aggregate before commit.
	quotaAuthorize(t, s, f, "aggregate", now)
	event.RequestID = "aggregate"
	event.EventID = "aggregate-overflow"
	if _, err = s.RecordUsageEventBilled(ctx, event, "", &cost, "", ""); err == nil {
		t.Fatal("aggregate overflow accepted")
	}
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM billed_event_receipts WHERE event_id='aggregate-overflow'", 0)
}

func TestQuotaPolicyRestartAndStableAssignment(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s, f := quotaTestFixture(t, &now)
	limit := 3
	if err := s.SetAccountConcurrency(ctx, f.assignment.AuthID, &limit, nil); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMemberLimits(ctx, f.membership.ID, domain.MemberLimitsUpdate{UserConcurrencySet: true, UserConcurrency: &limit}, nil); err != nil {
		t.Fatal(err)
	}
	otherCar, err := s.CreateCar(ctx, domain.Car{Name: "other", Status: domain.CarStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{AuthID: f.assignment.AuthID, CarID: otherCar.ID, SafeLabel: "other", ProviderSnapshot: "codex", CreatedByUserID: f.membership.CreatedByUserID}, ExpectedCurrentID: f.assignment.ID})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{AuthID: f.assignment.AuthID, CarID: f.car.ID, SafeLabel: "returned", ProviderSnapshot: "codex", CreatedByUserID: f.membership.CreatedByUserID}, ExpectedCurrentID: moved.ID})
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.GetAccountConcurrency(ctx, f.assignment.AuthID)
	if err != nil || got == nil || *got != limit {
		t.Fatalf("stable account: %v %v", got, err)
	}
	key, err := s.CreateAPIKey(ctx, domain.APIKey{UserID: f.user.ID, Name: "second", SecretDigest: []byte("second")})
	if err != nil {
		t.Fatal(err)
	}
	got, err = s.GetUserConcurrency(ctx, key.UserID)
	if err != nil || got == nil || *got != limit {
		t.Fatalf("stable user: %v %v", got, err)
	}
	quotaObserve(t, s, f, "5h", now.Add(-time.Hour), now.Add(4*time.Hour), now)
	quotaAuthorize(t, s, f, "persisted", now)
	cost := int64(20)
	quotaCharge(t, s, f, "persisted", "persisted", &cost)
	// Close all connections before reopening to verify actual restart persistence.
	var path string
	var seq int
	var name string
	if err = s.db.QueryRowContext(ctx, "PRAGMA database_list").Scan(&seq, &name, &path); err != nil {
		t.Fatal(err)
	}
	closeTestStore(t, s)
	reopened := openTestStore(t, path, func() time.Time { return now })
	defer closeTestStore(t, reopened)
	if view := quotaView(t, reopened, f, now)[0]; view.ConfirmedNanoUSD != cost {
		t.Fatalf("restart: %+v", view)
	}
	got, err = reopened.GetAccountConcurrency(ctx, f.assignment.AuthID)
	if err != nil || got == nil || *got != limit {
		t.Fatalf("restart policy: %v %v", got, err)
	}
	for _, bad := range []int{0, -1} {
		if err = reopened.SetAccountConcurrency(ctx, f.assignment.AuthID, &bad, nil); !errors.Is(err, domain.ErrInvalid) {
			t.Fatalf("invalid account limit: %v", err)
		}
	}
	if err = reopened.SetAccountConcurrency(ctx, f.assignment.AuthID, nil, nil); err != nil {
		t.Fatal(err)
	}
	got, err = reopened.GetAccountConcurrency(ctx, f.assignment.AuthID)
	if err != nil || got != nil {
		t.Fatalf("clear account: %v %v", got, err)
	}
}

func TestQuotaMigrationUpgradeDefaultsAndChecksums(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "upgrade.db")
	if err := prepareDatabaseFile(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	old := &Store{db: db, now: func() time.Time { return now }}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if err = old.applyMigrations(ctx, migrations[:3]); err != nil {
		t.Fatal(err)
	}
	f := newRetentionFixture(t, ctx, old)
	month := int64(17)
	if _, err = old.SetMonthlyLimit(ctx, f.membership.ID, &month, nil); err != nil {
		t.Fatal(err)
	}
	closeTestStore(t, old)
	upgraded := openTestStore(t, path, func() time.Time { return now })
	defer closeTestStore(t, upgraded)
	limits, err := upgraded.GetMemberQuotaLimits(ctx, f.membership.ID)
	if err != nil || limits.MonthlyNanoUSD == nil || *limits.MonthlyNanoUSD != month || limits.FiveHourNanoUSD != nil || limits.WeeklyNanoUSD != nil || limits.UserConcurrency != nil {
		t.Fatalf("upgrade defaults: %+v %v", limits, err)
	}
	for i, want := range []string{migration001Checksum, migration002Checksum, migration003Checksum, migration004Checksum} {
		var got string
		if err = upgraded.db.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version=?`, i+1).Scan(&got); err != nil || got != want {
			t.Fatalf("checksum %d: %s %v", i+1, got, err)
		}
	}
}

func TestQuotaObservationValidationAtomicity(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s, f := quotaTestFixture(t, &now)
	valid := domain.QuotaWindowObservation{AuthID: f.assignment.AuthID, Kind: "5h", From: now.Add(-time.Hour), ResetAt: now.Add(4 * time.Hour), ObservedAt: now}
	for i, mutate := range []func(*domain.QuotaWindowObservation){func(o *domain.QuotaWindowObservation) { o.Kind = "primary" }, func(o *domain.QuotaWindowObservation) { o.AuthID = "" }, func(o *domain.QuotaWindowObservation) { o.ResetAt = o.From }, func(o *domain.QuotaWindowObservation) { o.ObservedAt = time.Time{} }} {
		t.Run(fmt.Sprint(i), func(t *testing.T) {
			bad := valid
			mutate(&bad)
			if err := s.ObserveQuotaWindows(ctx, []domain.QuotaWindowObservation{valid, bad}); !errors.Is(err, domain.ErrInvalid) {
				t.Fatalf("invalid observation: %v", err)
			}
		})
	}
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM quota_windows", 0)
}

func TestQuotaExpiredPendingChargesJoinNextObservedWindow(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s, f := quotaTestFixture(t, &now)
	reset := now.Add(time.Hour)
	quotaObserve(t, s, f, "5h", reset.Add(-5*time.Hour), reset, now)
	quotaAuthorize(t, s, f, "old-long", now)
	oldCost := int64(13)
	quotaCharge(t, s, f, "old-long", "old-start", &oldCost)
	now = reset.Add(time.Minute)
	quotaAuthorize(t, s, f, "pending-new", now)
	cost := int64(23)
	event := quotaCharge(t, s, f, "pending-new", "pending-new", &cost)
	if view := quotaView(t, s, f, now)[0]; !view.PendingSync {
		t.Fatalf("invented next period: %+v", view)
	}
	quotaObserve(t, s, f, "5h", reset, reset.Add(5*time.Hour), now)
	quotaCharge(t, s, f, "old-long", "old-late", &oldCost)
	if view := quotaView(t, s, f, now)[0]; view.PendingSync || view.ConfirmedNanoUSD != cost {
		t.Fatalf("pending settlement: %+v", view)
	}
	// A late duplicate with altered supplied cost cannot reprice the event.
	if _, err := s.RecordUsageEventBilled(ctx, event, "", &oldCost, "priced", ""); err != nil {
		t.Fatal(err)
	}
	if view := quotaView(t, s, f, now)[0]; view.ConfirmedNanoUSD != cost {
		t.Fatalf("replay settlement: %+v", view)
	}
	oldView := quotaView(t, s, f, reset.Add(-time.Minute))[0]
	if oldView.ConfirmedNanoUSD != oldCost*2 {
		t.Fatalf("late request moved windows: %+v", oldView)
	}
}

func TestQuotaMembersShareBoundariesNotChargesAndRebindingPreservesFees(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s, f := quotaTestFixture(t, &now)
	quotaObserve(t, s, f, "5h", now.Add(-time.Hour), now.Add(4*time.Hour), now)
	quotaAuthorize(t, s, f, "original", now)
	cost := int64(29)
	quotaCharge(t, s, f, "original", "original", &cost)
	secondUser, err := s.CreateUser(ctx, testUser("second-passenger", domain.UserRolePassenger))
	if err != nil {
		t.Fatal(err)
	}
	secondMember, err := s.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{UserID: secondUser.ID, CarID: f.car.ID, DisplayName: "Second", CreatedByUserID: f.membership.CreatedByUserID}})
	if err != nil {
		t.Fatal(err)
	}
	otherViews, err := s.MemberQuotaWindows(ctx, secondMember.ID, f.assignment.AuthID, now)
	if err != nil {
		t.Fatal(err)
	}
	firstView := quotaView(t, s, f, now)[0]
	if otherViews[0].ConfirmedNanoUSD != 0 || otherViews[0].PendingSync || !otherViews[0].ResetAt.Equal(firstView.ResetAt) {
		t.Fatalf("member isolation: first=%+v second=%+v", firstView, otherViews[0])
	}
	otherCar, err := s.CreateCar(ctx, domain.Car{Name: "temporary", Status: domain.CarStatusActive})
	if err != nil {
		t.Fatal(err)
	}
	moved, err := s.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{AuthID: f.assignment.AuthID, CarID: otherCar.ID, SafeLabel: "temporary", ProviderSnapshot: "codex", CreatedByUserID: f.membership.CreatedByUserID}, ExpectedCurrentID: f.assignment.ID})
	if err != nil {
		t.Fatal(err)
	}
	returned, err := s.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{AuthID: f.assignment.AuthID, CarID: f.car.ID, SafeLabel: "returned", ProviderSnapshot: "codex", CreatedByUserID: f.membership.CreatedByUserID}, ExpectedCurrentID: moved.ID})
	if err != nil {
		t.Fatal(err)
	}
	if returned.ID == f.assignment.ID {
		t.Fatal("fixture did not replace assignment")
	}
	if view := quotaView(t, s, f, now)[0]; view.ConfirmedNanoUSD != cost || !view.ResetAt.Equal(firstView.ResetAt) {
		t.Fatalf("assignment reset fees: %+v", view)
	}
	otherViews, err = s.MemberQuotaWindows(ctx, f.membership.ID, "different-auth", now)
	if err != nil || !otherViews[0].PendingSync {
		t.Fatalf("new account inherited old window: %+v %v", otherViews, err)
	}
}

func TestQuotaAccountingFailureRollsBackAllLedgers(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 15, 12, 0, 0, 0, time.UTC)
	s, f := quotaTestFixture(t, &now)
	quotaObserve(t, s, f, "5h", now.Add(-time.Hour), now.Add(4*time.Hour), now)
	quotaAuthorize(t, s, f, "atomic", now)
	if _, err := s.db.ExecContext(ctx, `CREATE TRIGGER fail_monthly_fee BEFORE UPDATE OF confirmed_nano_usd ON billing_periods BEGIN SELECT RAISE(ABORT,'monthly write failure'); END`); err != nil {
		t.Fatal(err)
	}
	cost := int64(3)
	event := domain.UsageEvent{EventID: "atomic", RequestID: "atomic", AuthID: f.assignment.AuthID, Model: "test", UsageKnown: true}
	if _, err := s.RecordUsageEventBilled(ctx, event, "", &cost, "", ""); err == nil {
		t.Fatal("failed monthly write succeeded")
	}
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM billed_event_receipts", 0)
	assertRetentionRowCount(t, s, "SELECT COUNT(*) FROM usage_events", 0)
	if view := quotaView(t, s, f, now)[0]; view.ConfirmedNanoUSD != 0 {
		t.Fatalf("rolled back fee: %+v", view)
	}
	if _, err := s.db.ExecContext(ctx, `DROP TRIGGER fail_monthly_fee`); err != nil {
		t.Fatal(err)
	}
	quotaCharge(t, s, f, "atomic", "atomic", &cost)
	if view := quotaView(t, s, f, now)[0]; view.ConfirmedNanoUSD != cost {
		t.Fatalf("retry fee: %+v", view)
	}
}
