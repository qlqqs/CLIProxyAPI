package sqlite

import (
	"context"
	"database/sql"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestLocalQuotaWindowsExpireAndRestartDeterministically(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "quota.db"), Now: func() time.Time { return now }, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	f := newRetentionFixture(t, ctx, store)
	five, week := int64(10_000_000_000), int64(50_000_000_000)
	if err = store.SetMemberLimits(ctx, f.membership.ID, domain.MemberLimitsUpdate{FiveHourSet: true, FiveHourNanoUSD: &five, WeeklySet: true, WeeklyNanoUSD: &week}, nil); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.ExecContext(ctx, `INSERT INTO member_quota_windows(membership_id,kind,window_from,reset_at,confirmed_nano_usd,updated_at) VALUES(?,?,?,?,?,?),(?,?,?,?,?,?)`, f.membership.ID, "5h", toDatabaseTime(now), toDatabaseTime(now.Add(5*time.Hour)), 2_000_000_000, toDatabaseTime(now), f.membership.ID, "7d", toDatabaseTime(now.Truncate(24*time.Hour)), toDatabaseTime(now.Truncate(24*time.Hour).Add(7*24*time.Hour)), 2_000_000_000, toDatabaseTime(now)); err != nil {
		t.Fatal(err)
	}
	windows, err := store.MemberQuotaWindows(ctx, f.membership.ID, now)
	if err != nil || windows[0].ConfirmedNanoUSD != 2_000_000_000 || windows[1].ConfirmedNanoUSD != 2_000_000_000 {
		t.Fatalf("windows=%+v err=%v", windows, err)
	}
	windows, err = store.MemberQuotaWindows(ctx, f.membership.ID, now.Add(8*24*time.Hour))
	if err != nil || !windows[0].From.IsZero() || !windows[1].From.IsZero() || windows[0].ConfirmedNanoUSD != 0 || windows[1].ConfirmedNanoUSD != 0 {
		t.Fatalf("expired=%+v err=%v", windows, err)
	}
}

func TestLocalQuotaMigrationPreservesLegacyColumns(t *testing.T) {
	ctx := context.Background()
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "migration.db")})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	version, err := store.SchemaVersion(ctx)
	if err != nil || version != 6 {
		t.Fatalf("version=%d err=%v", version, err)
	}
	for _, table := range []string{"billing_periods", "quota_request_fees", "member_quota_windows"} {
		var count int
		if err = store.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, table).Scan(&count); err != nil || count != 1 {
			t.Fatalf("table %s count=%d err=%v", table, count, err)
		}
	}
}

func TestReliableFeeAccumulatesBothLocalWindowsAndResetClearsState(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 30, 0, 0, time.UTC)
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "billing.db"), Now: func() time.Time { return now }, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := newRetentionFixture(t, ctx, store)
	limit := int64(1_000_000_000)
	if err = store.SetMemberLimits(ctx, fixture.membership.ID, domain.MemberLimitsUpdate{FiveHourSet: true, FiveHourNanoUSD: &limit, WeeklySet: true, WeeklyNanoUSD: &limit}, nil); err != nil {
		t.Fatal(err)
	}
	completed := now.Add(time.Minute)
	seedRetentionRequest(t, ctx, store, fixture, "local-fee", now, &completed)
	cost := int64(1_250_000_000)
	if _, err = store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "local-fee-event", RequestID: "local-fee", AuthID: fixture.assignment.AuthID, UsageKnown: true, Provider: "codex", Model: "test", RequestedAt: now}, "", &cost, "priced", ""); err != nil {
		t.Fatal(err)
	}
	windows, err := store.MemberQuotaWindows(ctx, fixture.membership.ID, now)
	if err != nil || windows[0].ConfirmedNanoUSD != cost || windows[1].ConfirmedNanoUSD != cost {
		t.Fatalf("windows=%+v err=%v", windows, err)
	}
	preview, err := store.PreviewRetentionJob(ctx, "reset_quota_windows", fixture.user.UserRef, now)
	if err != nil || preview.ExpectedCount != 2 {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	job, err := store.ConfirmRetentionJob(ctx, preview.ID, preview.Operation, preview.ActorRef)
	if err != nil || job.DeletedCount != 2 {
		t.Fatalf("job=%+v err=%v", job, err)
	}
	windows, err = store.MemberQuotaWindows(ctx, fixture.membership.ID, now)
	if err != nil || !windows[0].From.IsZero() || !windows[1].From.IsZero() {
		t.Fatalf("reset windows=%+v err=%v", windows, err)
	}
	var billed int64
	if err = store.db.QueryRowContext(ctx, `SELECT billed_nano_usd FROM proxy_requests WHERE request_id='local-fee'`).Scan(&billed); err != nil || billed != cost {
		t.Fatalf("historical billed=%d err=%v", billed, err)
	}
}

func TestUnknownFeeDoesNotStartOrRestartLocalWindows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "unknown.db"), Now: func() time.Time { return now }, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := newRetentionFixture(t, ctx, store)
	completed := now.Add(time.Minute)
	seedRetentionRequest(t, ctx, store, fixture, "unknown-first", now, &completed)
	if _, err = store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "unknown-first-event", RequestID: "unknown-first", AuthID: fixture.assignment.AuthID, UsageKnown: false, Model: "test", RecordedAt: completed}, "", nil, "unknown", "missing usage"); err != nil {
		t.Fatal(err)
	}
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM member_quota_windows`, 0)

	zero := int64(0)
	seedRetentionRequest(t, ctx, store, fixture, "reliable-zero", now.Add(time.Minute), &completed)
	if _, err = store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "reliable-zero-event", RequestID: "reliable-zero", AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: completed}, "", &zero, "priced", ""); err != nil {
		t.Fatal(err)
	}
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM member_quota_windows`, 2)

	later := now.Add(8 * 24 * time.Hour)
	seedRetentionRequest(t, ctx, store, fixture, "unknown-after-expiry", later, &later)
	if _, err = store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "unknown-after-expiry-event", RequestID: "unknown-after-expiry", AuthID: fixture.assignment.AuthID, UsageKnown: false, Model: "test", RecordedAt: later}, "", nil, "unknown", "missing usage"); err != nil {
		t.Fatal(err)
	}
	windows, err := store.MemberQuotaWindows(ctx, fixture.membership.ID, later)
	if err != nil || !windows[0].From.IsZero() || !windows[1].From.IsZero() {
		t.Fatalf("unknown fee restarted expired windows: %+v err=%v", windows, err)
	}
}

func TestWeeklyLocalWindowUsesCalendarDaysAcrossDST(t *testing.T) {
	location, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	started := time.Date(2026, 10, 31, 16, 0, 0, 0, location)
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "dst.db"), Now: func() time.Time { return started }, Location: location})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := newRetentionFixture(t, ctx, store)
	seedRetentionRequest(t, ctx, store, fixture, "dst", started, &started)
	cost := int64(1)
	if _, err = store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "dst-event", RequestID: "dst", AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: started}, "", &cost, "priced", ""); err != nil {
		t.Fatal(err)
	}
	windows, err := store.MemberQuotaWindows(ctx, fixture.membership.ID, started)
	if err != nil {
		t.Fatal(err)
	}
	weekly := windows[1]
	wantFrom := time.Date(2026, 10, 31, 0, 0, 0, 0, location)
	wantReset := time.Date(2026, 11, 7, 0, 0, 0, 0, location)
	if !weekly.From.Equal(wantFrom) || !weekly.ResetAt.Equal(wantReset) || weekly.ResetAt.Sub(weekly.From) != 169*time.Hour {
		t.Fatalf("weekly DST window = %s..%s (%s), want %s..%s (169h)", weekly.From, weekly.ResetAt, weekly.ResetAt.Sub(weekly.From), wantFrom, wantReset)
	}
}

func TestOutOfOrderReliableFeesPreserveNewerWindow(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "out-of-order.db"), Now: func() time.Time { return base }, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := newRetentionFixture(t, ctx, store)
	for _, item := range []struct {
		id      string
		started time.Time
		cost    int64
	}{{"newer", base.Add(time.Hour), 20}, {"overlapping-older", base, 10}, {"expired-old", base.Add(-8 * 24 * time.Hour), 30}} {
		seedRetentionRequest(t, ctx, store, fixture, item.id, item.started, &item.started)
		if _, err = store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: item.id + "-event", RequestID: item.id, AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: base.Add(2 * time.Hour)}, "", &item.cost, "priced", ""); err != nil {
			t.Fatal(err)
		}
	}
	windows, err := store.MemberQuotaWindows(ctx, fixture.membership.ID, base.Add(2*time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if windows[0].ConfirmedNanoUSD != 30 || !windows[0].From.Equal(base) || !windows[0].ResetAt.Equal(base.Add(5*time.Hour)) {
		t.Fatalf("5h out-of-order window = %+v", windows[0])
	}
	if windows[1].ConfirmedNanoUSD != 30 {
		t.Fatalf("7d out-of-order window = %+v", windows[1])
	}
	var oldBilled int64
	if err = store.db.QueryRowContext(ctx, `SELECT billed_nano_usd FROM proxy_requests WHERE request_id='expired-old'`).Scan(&oldBilled); err != nil || oldBilled != 30 {
		t.Fatalf("late historical billing = %d err=%v", oldBilled, err)
	}
}

func TestReliableFeeIdempotencyAndWindowFailureRollback(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "atomic.db"), Now: func() time.Time { return now }, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := newRetentionFixture(t, ctx, store)
	seedRetentionRequest(t, ctx, store, fixture, "idempotent", now, &now)
	cost := int64(7)
	event := domain.UsageEvent{EventID: "same-event", RequestID: "idempotent", AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: now}
	for range 2 {
		if _, err = store.RecordUsageEventBilled(ctx, event, "", &cost, "priced", ""); err != nil {
			t.Fatal(err)
		}
	}
	windows, err := store.MemberQuotaWindows(ctx, fixture.membership.ID, now)
	if err != nil || windows[0].ConfirmedNanoUSD != cost || windows[1].ConfirmedNanoUSD != cost {
		t.Fatalf("duplicate accumulated twice: %+v err=%v", windows, err)
	}
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM billed_event_receipts WHERE event_id='same-event'`, 1)

	seedRetentionRequest(t, ctx, store, fixture, "rollback", now.Add(time.Minute), &now)
	if _, err = store.db.ExecContext(ctx, `CREATE TRIGGER fail_local_quota BEFORE UPDATE ON member_quota_windows BEGIN SELECT RAISE(ABORT,'local quota failure'); END`); err != nil {
		t.Fatal(err)
	}
	if _, err = store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "rollback-event", RequestID: "rollback", AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: now}, "", &cost, "priced", ""); err == nil {
		t.Fatal("local quota failure committed billing facts")
	}
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM billed_event_receipts WHERE event_id='rollback-event'`, 0)
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM usage_events WHERE event_id='rollback-event'`, 0)
	var billed *int64
	if err = store.db.QueryRowContext(ctx, `SELECT billed_nano_usd FROM proxy_requests WHERE request_id='rollback'`).Scan(&billed); err != nil || billed != nil {
		t.Fatalf("request billing survived rollback: %v err=%v", billed, err)
	}
}

func TestResetQuotaWindowsIncludesZeroCounterRows(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "zero-reset.db"), Now: func() time.Time { return now }, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	fixture := newRetentionFixture(t, ctx, store)
	seedRetentionRequest(t, ctx, store, fixture, "zero", now, &now)
	zero := int64(0)
	if _, err = store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "zero-event", RequestID: "zero", AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: now}, "", &zero, "priced", ""); err != nil {
		t.Fatal(err)
	}
	preview, err := store.PreviewRetentionJob(ctx, "reset_quota_windows", fixture.user.UserRef, now)
	if err != nil || preview.ExpectedCount != 2 {
		t.Fatalf("zero-window preview = %+v err=%v", preview, err)
	}
	job, err := store.ConfirmRetentionJob(ctx, preview.ID, preview.Operation, preview.ActorRef)
	if err != nil || job.DeletedCount != 2 {
		t.Fatalf("zero-window reset = %+v err=%v", job, err)
	}
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM member_quota_windows`, 0)
}

func TestLocalQuotaUpgradePreservesActiveLegacyFees(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "upgrade.db")
	if err := prepareDatabaseFile(path); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout))
	if err != nil {
		t.Fatal(err)
	}
	old := &Store{db: db, now: func() time.Time { return now }, location: time.UTC}
	migrations, err := loadEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if err = old.applyMigrations(ctx, migrations[:5]); err != nil {
		t.Fatal(err)
	}
	fixture := newRetentionFixture(t, ctx, old)
	month := int64(17)
	if _, err = old.SetMonthlyLimit(ctx, fixture.membership.ID, &month, nil); err != nil {
		t.Fatal(err)
	}
	fiveFrom, fiveReset := now.Add(-time.Hour), now.Add(4*time.Hour)
	weekFrom, weekReset := now.Add(-24*time.Hour), now.Add(6*24*time.Hour)
	if _, err = old.db.ExecContext(ctx, `INSERT INTO quota_windows(id,auth_id,kind,window_from,reset_at,observed_at) VALUES(1,?,'5h',?,?,?),(2,?,'7d',?,?,?)`, fixture.assignment.AuthID, toDatabaseTime(fiveFrom), toDatabaseTime(fiveReset), toDatabaseTime(now), fixture.assignment.AuthID, toDatabaseTime(weekFrom), toDatabaseTime(weekReset), toDatabaseTime(now)); err != nil {
		t.Fatal(err)
	}
	if _, err = old.db.ExecContext(ctx, `INSERT INTO quota_request_fees(request_id,auth_id,membership_id,started_at,confirmed_nano_usd,unknown_cost_events,five_hour_window_id,weekly_window_id) VALUES('linked',?,?,?,100,1,1,2),('pending',?,?,?,50,2,NULL,NULL)`, fixture.assignment.AuthID, fixture.membership.ID, toDatabaseTime(now.Add(-30*time.Minute)), fixture.assignment.AuthID, fixture.membership.ID, toDatabaseTime(now.Add(-15*time.Minute))); err != nil {
		t.Fatal(err)
	}
	closeTestStore(t, old)

	upgraded, err := Open(ctx, Config{Path: path, Now: func() time.Time { return now }, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	member, err := upgraded.CurrentMembership(ctx, fixture.user.ID)
	if err != nil || member.MonthlyLimitNanoUSD == nil || *member.MonthlyLimitNanoUSD != month {
		t.Fatalf("legacy monthly field changed: %+v err=%v", member, err)
	}
	limits, err := upgraded.GetMemberQuotaLimits(ctx, fixture.membership.ID)
	if err != nil || limits.FiveHourNanoUSD == nil || *limits.FiveHourNanoUSD != 0 || limits.WeeklyNanoUSD == nil || *limits.WeeklyNanoUSD != 0 {
		t.Fatalf("upgraded limits = %+v err=%v", limits, err)
	}
	windows, err := upgraded.MemberQuotaWindows(ctx, fixture.membership.ID, now)
	if err != nil || windows[0].ConfirmedNanoUSD != 150 || windows[1].ConfirmedNanoUSD != 150 || windows[0].UnknownCostEvents != 3 || windows[1].UnknownCostEvents != 3 {
		t.Fatalf("legacy reliable fees lost: %+v err=%v", windows, err)
	}
}
