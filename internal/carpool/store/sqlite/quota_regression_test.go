package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestUpgradeRetainsRecentUnboundFees(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "legacy.db")
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
	if err := old.applyMigrations(ctx, migrations[:5]); err != nil {
		t.Fatal(err)
	}
	fixture := newRetentionFixture(t, ctx, old)
	_, err = db.ExecContext(ctx, `INSERT INTO quota_request_fees(request_id,auth_id,membership_id,started_at,confirmed_nano_usd,unknown_cost_events,five_hour_window_id,weekly_window_id) VALUES('old',?,?,?,100,0,NULL,NULL),('recent',?,?,?,50,0,NULL,NULL)`, fixture.assignment.AuthID, fixture.membership.ID, toDatabaseTime(now.Add(-10*24*time.Hour)), fixture.assignment.AuthID, fixture.membership.ID, toDatabaseTime(now.Add(-15*time.Minute)))
	if err != nil {
		t.Fatal(err)
	}
	closeTestStore(t, old)
	upgraded, err := Open(ctx, Config{Path: path, Now: func() time.Time { return now }, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestStore(t, upgraded) })
	windows, err := upgraded.MemberQuotaWindows(ctx, fixture.membership.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	for _, window := range windows {
		if window.ConfirmedNanoUSD != 50 {
			t.Errorf("%s: recent reliable cost lost on upgrade: got=%d want=50", window.Kind, window.ConfirmedNanoUSD)
		}
	}
}

func TestLateFeeDoesNotExpireNewerConsumption(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	now := base.Add(5*time.Hour + 30*time.Minute)
	store, err := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "late.db"), Now: func() time.Time { return now }, Location: time.UTC})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { closeTestStore(t, store) })
	fixture := newRetentionFixture(t, ctx, store)
	for _, item := range []struct {
		id      string
		started time.Time
		cost    int64
	}{
		{"first-completed", base.Add(time.Hour), 20},
		{"current-consumption", now, 30},
		{"late-older", base, 10},
	} {
		seedRetentionRequest(t, ctx, store, fixture, item.id, item.started, &now)
		_, err := store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: item.id + "-event", RequestID: item.id, AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: now}, "", &item.cost, "priced", "")
		if err != nil {
			t.Fatal(err)
		}
	}
	windows, err := store.MemberQuotaWindows(ctx, fixture.membership.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if windows[0].ConfirmedNanoUSD != 30 || windows[0].From.IsZero() {
		t.Fatalf("newer consumption was expired by an older late fee: %+v; want current cost=30", windows[0])
	}
}

func TestLateQuotaFeesAreOrderIndependentAcrossRollover(t *testing.T) {
	for _, order := range [][]int{{0, 1, 2}, {0, 2, 1}, {1, 0, 2}, {1, 2, 0}, {2, 0, 1}, {2, 1, 0}} {
		t.Run(fmt.Sprint(order), func(t *testing.T) {
			ctx := context.Background()
			base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
			now := base.Add(5*time.Hour + 30*time.Minute)
			store, errOpen := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "order.db"), Now: func() time.Time { return now }})
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			t.Cleanup(func() { closeTestStore(t, store) })
			fixture := newRetentionFixture(t, ctx, store)
			starts := []time.Time{base, base.Add(time.Hour), now}
			for _, index := range order {
				id := fmt.Sprintf("ordered-%d", index)
				seedRetentionRequest(t, ctx, store, fixture, id, starts[index], &now)
				cost := int64((index + 1) * 10)
				if _, errBill := store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: id, RequestID: id, AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: now}, "", &cost, "priced", ""); errBill != nil {
					t.Fatal(errBill)
				}
			}
			windows, errWindows := store.MemberQuotaWindows(ctx, fixture.membership.ID, now)
			if errWindows != nil || windows[0].ConfirmedNanoUSD != 30 || !windows[0].From.Equal(now) || windows[1].ConfirmedNanoUSD != 60 {
				t.Fatalf("order %v: windows=%+v err=%v", order, windows, errWindows)
			}
		})
	}
}

func TestQuotaAllocationsSurviveRetentionButNotExplicitReset(t *testing.T) {
	for _, resetMode := range []string{"confirmed", "legacy"} {
		t.Run(resetMode, func(t *testing.T) {
			ctx := context.Background()
			base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
			now := base.Add(6 * time.Hour)
			store, errOpen := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "retained.db"), Now: func() time.Time { return now }})
			if errOpen != nil {
				t.Fatal(errOpen)
			}
			t.Cleanup(func() { closeTestStore(t, store) })
			fixture := newRetentionFixture(t, ctx, store)
			bill := func(id string, started time.Time, cost int64) domain.UsageEvent {
				t.Helper()
				seedRetentionRequest(t, ctx, store, fixture, id, started, &now)
				event := domain.UsageEvent{EventID: id, RequestID: id, AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: now}
				if _, errBill := store.RecordUsageEventBilled(ctx, event, "", &cost, "priced", ""); errBill != nil {
					t.Fatal(errBill)
				}
				return event
			}
			event := bill("retained-fee", base.Add(time.Hour), 20)
			bill("newer-fee", base.Add(5*time.Hour+30*time.Minute), 30)
			if _, _, errDelete := store.ExecuteRetention(ctx, "usage_details", now.Add(time.Minute), 100); errDelete != nil {
				t.Fatal(errDelete)
			}
			assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM member_quota_window_fees`, 4)
			bill("late-fee", base, 10)
			windows, errWindows := store.MemberQuotaWindows(ctx, fixture.membership.ID, now)
			if errWindows != nil || windows[0].ConfirmedNanoUSD != 30 || windows[1].ConfirmedNanoUSD != 60 {
				t.Fatalf("retention lost quota facts: %+v err=%v", windows, errWindows)
			}
			if resetMode == "confirmed" {
				preview, errPreview := store.PreviewRetentionJob(ctx, "reset_quota_windows", fixture.user.UserRef, now)
				if errPreview != nil {
					t.Fatal(errPreview)
				}
				if _, errReset := store.ConfirmRetentionJob(ctx, preview.ID, preview.Operation, preview.ActorRef); errReset != nil {
					t.Fatal(errReset)
				}
			} else if _, _, errReset := store.ExecuteRetention(ctx, "reset_quota_windows", now, 100); errReset != nil {
				t.Fatal(errReset)
			}
			assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM member_quota_window_fees`, 0)
			bill("after-reset", now, 5)
			bill("after-reset-late", now.Add(-time.Minute), 7)
			duplicateCost := int64(20)
			if _, errReplay := store.RecordUsageEventBilled(ctx, event, "", &duplicateCost, "priced", ""); errReplay != nil {
				t.Fatal(errReplay)
			}
			windows, errWindows = store.MemberQuotaWindows(ctx, fixture.membership.ID, now)
			if errWindows != nil || windows[0].ConfirmedNanoUSD != 12 || windows[1].ConfirmedNanoUSD != 12 {
				t.Fatalf("reset/replay resurrected old fees: %+v err=%v", windows, errWindows)
			}
		})
	}
}

func TestQuotaAllocationFailureRollsBackReceiptAndBill(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, errOpen := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "allocation-failure.db"), Now: func() time.Time { return now }})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() { closeTestStore(t, store) })
	fixture := newRetentionFixture(t, ctx, store)
	seedRetentionRequest(t, ctx, store, fixture, "atomic-allocation", now, &now)
	if _, errTrigger := store.db.ExecContext(ctx, `CREATE TRIGGER fail_allocation BEFORE INSERT ON member_quota_window_fees BEGIN SELECT RAISE(ABORT,'allocation failure'); END`); errTrigger != nil {
		t.Fatal(errTrigger)
	}
	cost := int64(1)
	if _, errBill := store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "allocation-event", RequestID: "atomic-allocation", AuthID: fixture.assignment.AuthID, Model: "test", UsageKnown: true, RecordedAt: now}, "", &cost, "priced", ""); errBill == nil {
		t.Fatal("allocation failure committed")
	}
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM billed_event_receipts`, 0)
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM usage_events`, 0)
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM member_quota_windows`, 0)
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE billed_nano_usd IS NOT NULL`, 0)
}

func TestQuotaFeeMigrationRecoversAvailableRequestStarts(t *testing.T) {
	ctx := context.Background()
	base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	now := base.Add(5*time.Hour + 30*time.Minute)
	path := filepath.Join(t.TempDir(), "v6.db")
	if errPrepare := prepareDatabaseFile(path); errPrepare != nil {
		t.Fatal(errPrepare)
	}
	db, errOpen := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	old := &Store{db: db, now: func() time.Time { return now }, location: time.UTC}
	migrations, errMigrations := loadEmbeddedMigrations()
	if errMigrations != nil {
		t.Fatal(errMigrations)
	}
	if errApply := old.applyMigrations(ctx, migrations[:6]); errApply != nil {
		t.Fatal(errApply)
	}
	fixture := newRetentionFixture(t, ctx, old)
	for _, item := range []struct {
		id      string
		started time.Time
		cost    int64
	}{{"v6-first", base.Add(time.Hour), 20}, {"v6-newer", now, 30}} {
		seedRetentionRequest(t, ctx, old, fixture, item.id, item.started, &now)
		if _, errReceipt := db.ExecContext(ctx, `INSERT INTO billed_event_receipts(event_id,request_id,cost_nano_usd,pricing_status,pricing_reason) VALUES(?,?,?,'priced','')`, item.id, item.id, item.cost); errReceipt != nil {
			t.Fatal(errReceipt)
		}
	}
	if _, errWindow := db.ExecContext(ctx, `INSERT INTO member_quota_windows(membership_id,kind,window_from,reset_at,confirmed_nano_usd,updated_at) VALUES(?,'5h',?,?,50,?)`, fixture.membership.ID, toDatabaseTime(base.Add(time.Hour)), toDatabaseTime(base.Add(6*time.Hour)), toDatabaseTime(now)); errWindow != nil {
		t.Fatal(errWindow)
	}
	closeTestStore(t, old)
	store, errUpgrade := Open(ctx, Config{Path: path, Now: func() time.Time { return now }, Location: time.UTC})
	if errUpgrade != nil {
		t.Fatal(errUpgrade)
	}
	t.Cleanup(func() { closeTestStore(t, store) })
	// An absent v6 window may have been explicitly reset. Do not resurrect it
	// from permanent receipts during migration.
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM member_quota_windows WHERE kind='7d'`, 0)
	seedRetentionRequest(t, ctx, store, fixture, "v7-late", base, &now)
	cost := int64(10)
	if _, errBill := store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "v7-late", RequestID: "v7-late", AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: now}, "", &cost, "priced", ""); errBill != nil {
		t.Fatal(errBill)
	}
	windows, errWindows := store.MemberQuotaWindows(ctx, fixture.membership.ID, now)
	if errWindows != nil || windows[0].ConfirmedNanoUSD != 30 || !windows[0].From.Equal(now) || windows[1].ConfirmedNanoUSD != 10 {
		t.Fatalf("migration lost request starts or restored reset fees: %+v err=%v", windows, errWindows)
	}
}

func TestQuotaFeeMigrationPreservesPrunedAggregate(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "pruned-v6.db")
	if errPrepare := prepareDatabaseFile(path); errPrepare != nil {
		t.Fatal(errPrepare)
	}
	db, errOpen := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout))
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	old := &Store{db: db, now: func() time.Time { return now }, location: time.UTC}
	migrations, errMigrations := loadEmbeddedMigrations()
	if errMigrations != nil {
		t.Fatal(errMigrations)
	}
	if errApply := old.applyMigrations(ctx, migrations[:6]); errApply != nil {
		t.Fatal(errApply)
	}
	fixture := newRetentionFixture(t, ctx, old)
	if _, errWindow := db.ExecContext(ctx, `INSERT INTO member_quota_windows(membership_id,kind,window_from,reset_at,confirmed_nano_usd,unknown_cost_events,updated_at) VALUES(?,'5h',?,?,40,2,?)`, fixture.membership.ID, toDatabaseTime(now), toDatabaseTime(now.Add(5*time.Hour)), toDatabaseTime(now)); errWindow != nil {
		t.Fatal(errWindow)
	}
	closeTestStore(t, old)
	store, errUpgrade := Open(ctx, Config{Path: path, Now: func() time.Time { return now }, Location: time.UTC})
	if errUpgrade != nil {
		t.Fatal(errUpgrade)
	}
	t.Cleanup(func() { closeTestStore(t, store) })
	seedRetentionRequest(t, ctx, store, fixture, "pruned-late", now.Add(-time.Hour), &now)
	cost := int64(10)
	if _, errBill := store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "pruned-late", RequestID: "pruned-late", AuthID: fixture.assignment.AuthID, UsageKnown: true, Model: "test", RecordedAt: now}, "", &cost, "priced", ""); errBill != nil {
		t.Fatal(errBill)
	}
	windows, errWindows := store.MemberQuotaWindows(ctx, fixture.membership.ID, now)
	if errWindows != nil || windows[0].ConfirmedNanoUSD != 50 || windows[0].UnknownCostEvents != 2 {
		t.Fatalf("migration discarded pruned aggregate: %+v err=%v", windows, errWindows)
	}
}

func TestLateHistoricalFeePreservesWeeklyUnknownBeforeFirstReliableStart(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
	store, errOpen := Open(ctx, Config{Path: filepath.Join(t.TempDir(), "weekly-unknown.db"), Now: func() time.Time { return now }, Location: time.UTC})
	if errOpen != nil {
		t.Fatal(errOpen)
	}
	t.Cleanup(func() { closeTestStore(t, store) })
	fixture := newRetentionFixture(t, ctx, store)
	cost := int64(10)
	for _, item := range []struct {
		id      string
		started time.Time
		cost    *int64
	}{
		{"reliable-noon", now, &cost},
		{"unknown-morning", now.Add(-6 * time.Hour), nil},
		{"historical-reliable", now.AddDate(0, 0, -8), &cost},
	} {
		seedRetentionRequest(t, ctx, store, fixture, item.id, item.started, &now)
		if _, errBill := store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: item.id, RequestID: item.id, AuthID: fixture.assignment.AuthID, UsageKnown: item.cost != nil, Model: "test", RecordedAt: now}, "", item.cost, "", ""); errBill != nil {
			t.Fatal(errBill)
		}
		if item.cost == nil || item.id == "historical-reliable" {
			windows, errWindows := store.MemberQuotaWindows(ctx, fixture.membership.ID, now)
			if errWindows != nil || windows[1].ConfirmedNanoUSD != cost || windows[1].UnknownCostEvents != 1 {
				t.Fatalf("after %s: weekly window=%+v err=%v; want cost=10 and unknown=1", item.id, windows, errWindows)
			}
		}
	}
}

func TestQuotaRolloverIncludesPersistedUnknownAllocations(t *testing.T) {
	for _, kind := range []string{domain.QuotaWindowFiveHour, domain.QuotaWindowWeekly} {
		for _, cost := range []int64{0, 10} {
			t.Run(fmt.Sprintf("%s/cost=%d", kind, cost), func(t *testing.T) {
				ctx := context.Background()
				base := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
				started := base.Add(5*time.Hour + 30*time.Minute)
				unknownStarted := started.Add(30 * time.Minute)
				wantFrom, wantReset := started, started.Add(5*time.Hour)
				index := 0
				if kind == domain.QuotaWindowWeekly {
					started = base.AddDate(0, 0, 7)
					wantFrom = time.Date(2026, 9, 26, 0, 0, 0, 0, time.UTC)
					wantReset = wantFrom.AddDate(0, 0, 7)
					unknownStarted = wantFrom.Add(6 * time.Hour)
					index = 1
				}
				now := started.Add(time.Hour)
				store := openTestStore(t, filepath.Join(t.TempDir(), "unknown-rollover.db"), func() time.Time { return now })
				t.Cleanup(func() { closeTestStore(t, store) })
				fixture := newRetentionFixture(t, ctx, store)
				initialCost := int64(20)
				for _, item := range []struct {
					id      string
					started time.Time
					cost    *int64
				}{
					{"initial", base, &initialCost},
					{"old-unknown", base.Add(time.Hour), nil},
					{"pending-unknown", unknownStarted, nil},
				} {
					seedRetentionRequest(t, ctx, store, fixture, item.id, item.started, &now)
					quotaCharge(t, store, fixture, item.id, item.id, item.cost)
				}
				if got := quotaView(t, store, fixture, now)[index]; !got.From.IsZero() || got.UnknownCostEvents != 0 {
					t.Fatalf("unknown allocation restarted expired window: %+v", got)
				}
				// The allocations, not retained request details, must supply the total.
				if _, _, errDelete := store.ExecuteRetention(ctx, "usage_details", now.Add(time.Minute), 100); errDelete != nil {
					t.Fatal(errDelete)
				}
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM member_quota_window_fees`, 6)
				seedRetentionRequest(t, ctx, store, fixture, "rollover", started, &now)
				for range 2 {
					quotaCharge(t, store, fixture, "rollover", "rollover", &cost)
					got := quotaView(t, store, fixture, now)[index]
					if got.ConfirmedNanoUSD != cost || got.UnknownCostEvents != 1 || !got.From.Equal(wantFrom) || !got.ResetAt.Equal(wantReset) {
						t.Fatalf("rollover/replay lost or duplicated allocations: %+v; want cost=%d unknown=1 bounds=%s..%s", got, cost, wantFrom, wantReset)
					}
				}
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM member_quota_window_fees`, 8)
			})
		}
	}
}

func TestQuotaUpgradePreservesLinkedWindowBoundsWithUnboundFees(t *testing.T) {
	for _, kind := range []string{domain.QuotaWindowFiveHour, domain.QuotaWindowWeekly} {
		for _, unboundCase := range []string{"unknown_only", "old_and_recent", "earlier_active"} {
			t.Run(kind+"/"+unboundCase, func(t *testing.T) {
				ctx := context.Background()
				now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)
				path := filepath.Join(t.TempDir(), "linked-v5.db")
				if errPrepare := prepareDatabaseFile(path); errPrepare != nil {
					t.Fatal(errPrepare)
				}
				db, errOpen := sql.Open("sqlite", sqliteDSN(path, defaultBusyTimeout))
				if errOpen != nil {
					t.Fatal(errOpen)
				}
				old := &Store{db: db, now: func() time.Time { return now }, location: time.UTC}
				t.Cleanup(func() { closeTestStore(t, old) })
				migrations, errMigrations := loadEmbeddedMigrations()
				if errMigrations != nil {
					t.Fatal(errMigrations)
				}
				if errApply := old.applyMigrations(ctx, migrations[:5]); errApply != nil {
					t.Fatal(errApply)
				}
				fixture := newRetentionFixture(t, ctx, old)
				firstFrom, firstReset := now.Add(-4*time.Hour-30*time.Minute), now.Add(30*time.Minute)
				secondFrom, secondReset := now.Add(-time.Hour), now.Add(4*time.Hour)
				column, index := "five_hour_window_id", 0
				if kind == domain.QuotaWindowWeekly {
					firstFrom = time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
					firstReset = firstFrom.AddDate(0, 0, 7)
					secondFrom = time.Date(2026, 9, 18, 0, 0, 0, 0, time.UTC)
					secondReset = secondFrom.AddDate(0, 0, 7)
					column, index = "weekly_window_id", 1
				}
				if unboundCase == "earlier_active" {
					firstFrom, secondFrom = now.Add(-30*time.Minute), now.Add(-15*time.Minute)
					firstReset, secondReset = firstFrom.Add(5*time.Hour), secondFrom.Add(5*time.Hour)
					if kind == domain.QuotaWindowWeekly {
						firstReset, secondReset = firstFrom.AddDate(0, 0, 7), secondFrom.AddDate(0, 0, 7)
					}
				}
				if _, errWindows := db.ExecContext(ctx, `INSERT INTO quota_windows(id,auth_id,kind,window_from,reset_at,observed_at) VALUES(1,?,?,?,?,?),(2,?,?,?,?,?)`, fixture.assignment.AuthID, kind, toDatabaseTime(firstFrom), toDatabaseTime(firstReset), toDatabaseTime(now), "second-auth", kind, toDatabaseTime(secondFrom), toDatabaseTime(secondReset), toDatabaseTime(now)); errWindows != nil {
					t.Fatal(errWindows)
				}
				query := `INSERT INTO quota_request_fees(request_id,auth_id,membership_id,started_at,confirmed_nano_usd,unknown_cost_events,` + column + `) VALUES('bound-first',?,?,?,10,1,1),('bound-second',?,?,?,20,2,2),('unbound-unknown',?,?,?,0,2,NULL),('outside-unknown',?,?,?,0,5,NULL)`
				if _, errFees := db.ExecContext(ctx, query, fixture.assignment.AuthID, fixture.membership.ID, toDatabaseTime(firstFrom.Add(time.Minute)), "second-auth", fixture.membership.ID, toDatabaseTime(now.Add(-10*time.Minute)), fixture.assignment.AuthID, fixture.membership.ID, toDatabaseTime(now.Add(-45*time.Minute)), fixture.assignment.AuthID, fixture.membership.ID, toDatabaseTime(now.AddDate(0, 0, -21))); errFees != nil {
					t.Fatal(errFees)
				}
				wantAmount, wantUnknown := int64(30), int64(5)
				if unboundCase != "unknown_only" {
					if _, errFees := db.ExecContext(ctx, `INSERT INTO quota_request_fees(request_id,auth_id,membership_id,started_at,confirmed_nano_usd,unknown_cost_events) VALUES('unbound-old',?,?,?,100,3),('unbound-recent',?,?,?,50,1)`, fixture.assignment.AuthID, fixture.membership.ID, toDatabaseTime(now.AddDate(0, 0, -14)), fixture.assignment.AuthID, fixture.membership.ID, toDatabaseTime(now.Add(-15*time.Minute))); errFees != nil {
						t.Fatal(errFees)
					}
					wantAmount, wantUnknown = 80, 6
				}
				if unboundCase == "earlier_active" {
					if _, errFee := db.ExecContext(ctx, `INSERT INTO quota_request_fees(request_id,auth_id,membership_id,started_at,confirmed_nano_usd,unknown_cost_events) VALUES('unbound-before-linked',?,?,?,10,1)`, fixture.assignment.AuthID, fixture.membership.ID, toDatabaseTime(now.Add(-time.Hour))); errFee != nil {
						t.Fatal(errFee)
					}
					wantAmount, wantUnknown = 90, 7
				}
				closeTestStore(t, old)
				store, errUpgrade := Open(ctx, Config{Path: path, Now: func() time.Time { return now }, Location: time.UTC})
				if errUpgrade != nil {
					t.Fatal(errUpgrade)
				}
				t.Cleanup(func() { closeTestStore(t, store) })
				got := quotaView(t, store, fixture, now)[index]
				if !got.From.Equal(firstFrom) || !got.ResetAt.Equal(secondReset) || got.ConfirmedNanoUSD != wantAmount || got.UnknownCostEvents != wantUnknown {
					t.Fatalf("linked migration changed bounds or lost unbound fees: %+v; want cost=%d unknown=%d bounds=%s..%s", got, wantAmount, wantUnknown, firstFrom, secondReset)
				}
			})
		}
	}
}
