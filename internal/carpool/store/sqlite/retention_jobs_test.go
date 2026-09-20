package sqlite

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func confirmedRetentionFixture(t *testing.T, now *time.Time) (*Store, retentionFixture, domain.BillingPeriod) {
	t.Helper()
	store := openTestStore(t, filepath.Join(t.TempDir(), "jobs.db"), func() time.Time { return *now })
	t.Cleanup(func() { closeTestStore(t, store) })
	fixture := newRetentionFixture(t, context.Background(), store)
	limit := int64(5_000_000_000)
	period, err := store.EnsureBillingPeriod(context.Background(), domain.BillingPeriod{MembershipID: fixture.membership.ID, MemberRefSnapshot: fixture.membership.MemberRef, CarID: fixture.car.ID, Timezone: "UTC", AnchorAt: now.Add(-24 * time.Hour), From: now.Add(-time.Hour), To: now.Add(time.Hour), LimitNanoUSD: &limit, ConfirmedNanoUSD: 225_000_000, UnknownCostEvents: 3})
	if err != nil {
		t.Fatal(err)
	}
	return store, fixture, period
}

func previewConfirmedRetention(t *testing.T, store *Store, operation string, cutoff time.Time) domain.RetentionJob {
	t.Helper()
	job, err := store.PreviewRetentionJob(context.Background(), operation, "usr_admin", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	if job.Status != "queued" || job.ID == "" || job.ExpectedCount > 1000 {
		t.Fatalf("invalid preview: %+v", job)
	}
	return job
}

func execRetentionSQL(t *testing.T, store *Store, query string, args ...any) {
	t.Helper()
	if _, err := store.db.ExecContext(context.Background(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestConfirmedRetentionResetConcurrentReplayPreservesNewUsage(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	store, fixture, period := confirmedRetentionFixture(t, &now)
	seedRetentionRequest(t, ctx, store, fixture, "prior", now.Add(-time.Minute), timePointer(now))
	execRetentionSQL(t, store, `UPDATE billing_periods SET confirmed_nano_usd=0 WHERE id=?`, period.ID)
	priorCost := int64(225_000_000)
	priorEvent := domain.UsageEvent{EventID: "prior-charge", RequestID: "prior", AuthID: fixture.assignment.AuthID, Provider: "codex", Model: "test", UsageKnown: true, RequestedAt: now}
	if _, err := store.RecordUsageEventBilled(ctx, priorEvent, period.ID, &priorCost, "priced", ""); err != nil {
		t.Fatal(err)
	}
	var err error
	period, err = store.GetBillingPeriod(ctx, fixture.membership.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	seedRetentionRequest(t, ctx, store, fixture, "in-flight", now, nil)
	execRetentionSQL(t, store, `UPDATE proxy_requests SET billing_period_id=? WHERE request_id='in-flight'`, period.ID)
	preview := previewConfirmedRetention(t, store, "reset_current_period", now)
	if preview.ExpectedCount != 1 || preview.InFlightCount != 1 {
		t.Fatalf("preview=%+v", preview)
	}
	start := make(chan struct{})
	var wg sync.WaitGroup
	results := make([]domain.RetentionJob, 2)
	errs := make([]error, 2)
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			results[i], errs[i] = store.ConfirmRetentionJob(ctx, preview.ID, preview.Operation, preview.ActorRef)
		}(i)
	}
	close(start)
	wg.Wait()
	for i, err := range errs {
		if err != nil || results[i].DeletedCount != 1 || results[i].Status != "completed" {
			t.Fatalf("confirm=%+v error=%v", results[i], err)
		}
	}
	if !reflect.DeepEqual(results[0], results[1]) {
		t.Fatal("duplicate confirmation changed the persisted result")
	}
	current, err := store.GetBillingPeriod(ctx, fixture.membership.ID, now)
	if err != nil {
		t.Fatal(err)
	}
	if current.ConfirmedNanoUSD != period.ConfirmedNanoUSD || current.ResetBaselineNanoUSD != period.ConfirmedNanoUSD || current.UnknownCostEvents != 0 || current.Revision != period.Revision+1 || !current.From.Equal(period.From) || !current.To.Equal(period.To) || !current.AnchorAt.Equal(period.AnchorAt) || *current.LimitNanoUSD != *period.LimitNanoUSD {
		t.Fatalf("reset changed contract: %+v", current)
	}
	if _, err = store.RecordUsageEventBilled(ctx, priorEvent, period.ID, &priorCost, "priced", ""); err != nil {
		t.Fatal(err)
	}
	cost := int64(75_000_000)
	event := domain.UsageEvent{EventID: "fresh-charge", UsageKnown: true, RequestID: "in-flight", AuthID: fixture.assignment.AuthID, Provider: "codex", Model: "test", RequestedAt: now}
	for range 2 {
		if _, err = store.RecordUsageEventBilled(ctx, event, period.ID, &cost, "priced", ""); err != nil {
			t.Fatal(err)
		}
	}
	unknown := event
	unknown.EventID = "fresh-unknown"
	if _, err = store.RecordUsageEventBilled(ctx, unknown, period.ID, nil, "unknown", ""); err != nil {
		t.Fatal(err)
	}
	now = now.Add(16 * time.Minute)
	duplicate, err := store.ConfirmRetentionJob(ctx, preview.ID, preview.Operation, preview.ActorRef)
	if err != nil || !reflect.DeepEqual(duplicate, results[0]) {
		t.Fatalf("completed replay=%+v err=%v", duplicate, err)
	}
	current, err = store.GetBillingPeriod(ctx, fixture.membership.ID, now)
	if err != nil || current.ConfirmedNanoUSD-current.ResetBaselineNanoUSD != 0 || current.UnknownCostEvents != 0 {
		t.Fatalf("replay erased fresh usage: %+v err=%v", current, err)
	}
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM audit_events WHERE target_ref=?`, 1, preview.ID)
	var metadata string
	if err = store.db.QueryRowContext(ctx, `SELECT metadata_json FROM audit_events WHERE target_ref=?`, preview.ID).Scan(&metadata); err != nil {
		t.Fatal(err)
	}
	var audit struct {
		Periods []retentionPeriodTarget `json:"periods"`
	}
	if err = json.Unmarshal([]byte(metadata), &audit); err != nil || len(audit.Periods) != 1 || audit.Periods[0].Confirmed != 0 || audit.Periods[0].Unknown != 3 || audit.Periods[0].ID != period.ID {
		t.Fatalf("audit=%s err=%v", metadata, err)
	}
}

func TestConfirmedRetentionRejectsInvalidOrStalePreview(t *testing.T) {
	for _, scenario := range []string{"expired", "cross_admin", "different_operation", "missing_job", "legacy_unbound", "revision", "amount_without_revision", "unknown_without_revision", "boundaries", "cross_period"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			store, fixture, period := confirmedRetentionFixture(t, &now)
			if scenario == "cross_period" {
				execRetentionSQL(t, store, `UPDATE billing_periods SET period_to=? WHERE id=?`, toDatabaseTime(now.Add(time.Minute)), period.ID)
			}
			preview := previewConfirmedRetention(t, store, "reset_current_period", now)
			id, op, actor := preview.ID, preview.Operation, preview.ActorRef
			switch scenario {
			case "expired":
				now = now.Add(domain.RetentionPreviewTTL)
			case "cross_admin":
				actor = "usr_other"
			case "different_operation":
				op = "closed_periods"
			case "missing_job":
				id = "unknown"
			case "legacy_unbound":
				execRetentionSQL(t, store, `UPDATE retention_jobs SET confirmation='' WHERE id=?`, id)
			case "revision":
				execRetentionSQL(t, store, `UPDATE billing_periods SET revision=revision+1 WHERE id=?`, period.ID)
			case "amount_without_revision":
				execRetentionSQL(t, store, `UPDATE billing_periods SET confirmed_nano_usd=confirmed_nano_usd+1 WHERE id=?`, period.ID)
			case "unknown_without_revision":
				execRetentionSQL(t, store, `UPDATE billing_periods SET unknown_cost_events=unknown_cost_events+1 WHERE id=?`, period.ID)
			case "boundaries":
				execRetentionSQL(t, store, `UPDATE billing_periods SET period_to=period_to+1 WHERE id=?`, period.ID)
			case "cross_period":
				now = now.Add(time.Minute)
			case "new_usage":
				seedRetentionRequest(t, ctx, store, fixture, "new", now, nil)
				cost := int64(1)
				if _, err := store.RecordUsageEventBilled(ctx, domain.UsageEvent{EventID: "new-event", UsageKnown: true, RequestID: "new", AuthID: fixture.assignment.AuthID, Provider: "codex", Model: "test"}, period.ID, &cost, "priced", ""); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.ConfirmRetentionJob(ctx, id, op, actor); !errors.Is(err, domain.ErrRetentionPreviewInvalid) {
				t.Fatalf("confirm err=%v", err)
			}
			assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM retention_jobs WHERE id=? AND status='queued' AND deleted_count=0 AND completed_at IS NULL`, 1, preview.ID)
			assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM billing_periods WHERE id=? AND reset_baseline_nano_usd=0`, 1, period.ID)
			assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM audit_events WHERE target_ref=?`, 0, preview.ID)
		})
	}
}

func TestConfirmedRetentionRollsBackMutationResultAndAudit(t *testing.T) {
	for _, operation := range []string{"usage_details", "reset_current_period", "closed_periods"} {
		for _, failure := range []string{"audit", "result", "mutation"} {
			t.Run(operation+"/"+failure, func(t *testing.T) {
				ctx := context.Background()
				now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
				store, fixture, period := confirmedRetentionFixture(t, &now)
				if operation == "closed_periods" {
					execRetentionSQL(t, store, `UPDATE billing_periods SET period_to=? WHERE id=?`, toDatabaseTime(now), period.ID)
				}
				seedRetentionRequest(t, ctx, store, fixture, "completed", now.Add(-time.Hour), timePointer(now.Add(-time.Minute)), now.Add(-time.Minute))
				execRetentionSQL(t, store, `UPDATE proxy_requests SET billing_period_id=? WHERE request_id='completed'`, period.ID)
				execRetentionSQL(t, store, `UPDATE usage_events SET billing_period_id=? WHERE request_id='completed'`, period.ID)
				preview := previewConfirmedRetention(t, store, operation, now)
				trigger := `CREATE TRIGGER fail_retention BEFORE INSERT ON audit_events BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`
				if failure == "result" {
					trigger = `CREATE TRIGGER fail_retention BEFORE UPDATE OF status ON retention_jobs BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`
				}
				if failure == "mutation" {
					switch operation {
					case "usage_details":
						trigger = `CREATE TRIGGER fail_retention BEFORE DELETE ON proxy_requests BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`
					case "closed_periods":
						trigger = `CREATE TRIGGER fail_retention BEFORE DELETE ON billing_periods BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`
					default:
						trigger = `CREATE TRIGGER fail_retention BEFORE UPDATE OF reset_baseline_nano_usd ON billing_periods BEGIN SELECT RAISE(ABORT,'synthetic failure'); END`
					}
				}
				execRetentionSQL(t, store, trigger)
				if _, err := store.ConfirmRetentionJob(ctx, preview.ID, operation, preview.ActorRef); err == nil {
					t.Fatal("failed transaction reported success")
				}
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM billing_periods WHERE id=? AND reset_baseline_nano_usd=0 AND unknown_cost_events=3`, 1, period.ID)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM usage_events WHERE request_id='completed' AND billing_period_id=?`, 1, period.ID)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE request_id='completed' AND billing_period_id=?`, 1, period.ID)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_request_auth_scopes WHERE request_id='completed'`, 1)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM retention_jobs WHERE id=? AND status='queued' AND completed_at IS NULL AND deleted_count=0`, 1, preview.ID)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM audit_events WHERE target_ref=?`, 0, preview.ID)
				execRetentionSQL(t, store, `DROP TRIGGER fail_retention`)
				job, err := store.ConfirmRetentionJob(ctx, preview.ID, operation, preview.ActorRef)
				if err != nil || job.DeletedCount != 1 || job.Status != "completed" {
					t.Fatalf("explicit retry=%+v err=%v", job, err)
				}
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM audit_events WHERE target_ref=?`, 1, preview.ID)
			})
		}
	}
}

func TestConfirmedRetentionFreezesRequestsAndDeletesWholeLargeRequest(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	store, fixture, period := confirmedRetentionFixture(t, &now)
	times := make([]time.Time, 1201)
	for i := range times {
		times[i] = now.Add(-time.Hour)
	}
	seedRetentionRequest(t, ctx, store, fixture, "large", now.Add(-time.Hour), timePointer(now.Add(-time.Minute)), times...)
	execRetentionSQL(t, store, `UPDATE proxy_requests SET billing_period_id=? WHERE request_id='large'`, period.ID)
	execRetentionSQL(t, store, `UPDATE usage_events SET billing_period_id=? WHERE request_id='large'`, period.ID)
	seedRetentionRequest(t, ctx, store, fixture, "later-completed", now.Add(-time.Hour), nil)
	seedRetentionRequest(t, ctx, store, fixture, "incomplete", now.Add(-time.Hour), timePointer(now.Add(-time.Minute)), now)
	execRetentionSQL(t, store, `UPDATE proxy_requests SET outcome='incomplete' WHERE request_id='incomplete'`)
	preview := previewConfirmedRetention(t, store, "usage_details", now)
	if preview.ExpectedCount != 1 || preview.InFlightCount != 2 {
		t.Fatalf("preview=%+v", preview)
	}
	seedRetentionRequest(t, ctx, store, fixture, "new-backdated", now.Add(-time.Hour), timePointer(now.Add(-time.Minute)))
	if err := store.CompleteProxyRequest(ctx, domain.RequestCompletion{RequestID: "later-completed", Outcome: domain.RequestOutcomeSucceeded, CompletedAt: now.Add(-time.Minute)}); err != nil {
		t.Fatal(err)
	}
	job, err := store.ConfirmRetentionJob(ctx, preview.ID, preview.Operation, preview.ActorRef)
	if err != nil || job.DeletedCount != 1 {
		t.Fatalf("confirm=%+v err=%v", job, err)
	}
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM usage_events WHERE request_id='large'`, 0)
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE request_id='large'`, 0)
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE request_id IN ('new-backdated','later-completed','incomplete')`, 3)
	current, err := store.GetBillingPeriod(ctx, fixture.membership.ID, now)
	if err != nil || !reflect.DeepEqual(current, period) {
		t.Fatalf("detail cleanup changed ledger: %+v err=%v", current, err)
	}
}

func TestConfirmedRetentionCapsEachOperationAt1000LogicalTargets(t *testing.T) {
	for _, operation := range []string{"usage_details", "closed_periods", "reset_current_period"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			store, fixture, period := confirmedRetentionFixture(t, &now)
			if operation != "usage_details" {
				execRetentionSQL(t, store, `DELETE FROM billing_periods WHERE id=?`, period.ID)
			}
			for i := range 1001 {
				if operation == "usage_details" {
					seedRetentionRequest(t, ctx, store, fixture, fmt.Sprintf("request-%04d", i), now.Add(-time.Hour), timePointer(now.Add(-time.Minute)))
				} else {
					from := now.Add(-time.Hour).Add(time.Duration(i) * time.Microsecond)
					to := now.Add(time.Hour)
					if operation == "closed_periods" {
						to = now
					}
					if _, err := store.EnsureBillingPeriod(ctx, domain.BillingPeriod{MembershipID: fixture.membership.ID, MemberRefSnapshot: fixture.membership.MemberRef, CarID: fixture.car.ID, Timezone: "UTC", AnchorAt: from, From: from, To: to, ConfirmedNanoUSD: 1}); err != nil {
						t.Fatal(err)
					}
				}
			}
			preview := previewConfirmedRetention(t, store, operation, now)
			if preview.ExpectedCount != 1000 {
				t.Fatalf("expected=%d", preview.ExpectedCount)
			}
			job, err := store.ConfirmRetentionJob(ctx, preview.ID, operation, preview.ActorRef)
			if err != nil || job.DeletedCount != 1000 {
				t.Fatalf("confirm=%+v err=%v", job, err)
			}
			switch operation {
			case "usage_details":
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests`, 1)
			case "closed_periods":
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM billing_periods`, 1)
			default:
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM billing_periods WHERE reset_baseline_nano_usd=0`, 1)
			}
			next := previewConfirmedRetention(t, store, operation, now)
			if next.ExpectedCount != 1 {
				t.Fatalf("next batch expected=%d, want remaining one", next.ExpectedCount)
			}
			remaining, err := store.ConfirmRetentionJob(ctx, next.ID, operation, next.ActorRef)
			if err != nil || remaining.DeletedCount != 1 {
				t.Fatalf("next batch=%+v err=%v", remaining, err)
			}
			if empty := previewConfirmedRetention(t, store, operation, now); empty.ExpectedCount != 0 {
				t.Fatalf("completed batches still selected targets: %+v", empty)
			}
		})
	}
}

func TestConfirmedRetentionClosedPeriodsProtectionAndRequestStaleness(t *testing.T) {
	for _, scenario := range []string{"closed_protected", "closed_new_protection", "closed_keeps_details", "request_incomplete", "request_new_event"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
			store, fixture, period := confirmedRetentionFixture(t, &now)
			execRetentionSQL(t, store, `UPDATE billing_periods SET period_to=? WHERE id=?`, toDatabaseTime(now), period.ID)
			seedRetentionRequest(t, ctx, store, fixture, "target", now.Add(-time.Hour), timePointer(now.Add(-time.Minute)), now.Add(-time.Minute))
			execRetentionSQL(t, store, `UPDATE proxy_requests SET billing_period_id=? WHERE request_id='target'`, period.ID)
			execRetentionSQL(t, store, `UPDATE usage_events SET billing_period_id=? WHERE request_id='target'`, period.ID)
			op := "closed_periods"
			if scenario == "request_incomplete" || scenario == "request_new_event" {
				op = "usage_details"
			}
			if scenario == "closed_protected" {
				execRetentionSQL(t, store, `UPDATE proxy_requests SET outcome='incomplete' WHERE request_id='target'`)
			}
			preview := previewConfirmedRetention(t, store, op, now)
			switch scenario {
			case "closed_protected":
				if preview.ExpectedCount != 0 {
					t.Fatalf("protected preview=%+v", preview)
				}
			case "closed_new_protection", "request_incomplete":
				execRetentionSQL(t, store, `UPDATE proxy_requests SET outcome='incomplete' WHERE request_id='target'`)
			case "request_new_event":
				if _, err := store.InsertUsageEvent(ctx, domain.UsageEvent{EventID: "late", RequestID: "target", AuthID: fixture.assignment.AuthID, Provider: "codex", Model: "test"}); err != nil {
					t.Fatal(err)
				}
			}
			job, err := store.ConfirmRetentionJob(ctx, preview.ID, op, preview.ActorRef)
			switch scenario {
			case "closed_keeps_details":
				if err != nil || job.DeletedCount != 1 {
					t.Fatalf("confirm=%+v err=%v", job, err)
				}
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM billing_periods`, 0)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE request_id='target' AND billing_period_id IS NULL`, 1)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM usage_events WHERE request_id='target' AND billing_period_id IS NULL`, 1)
			case "closed_protected":
				if err != nil || job.DeletedCount != 0 {
					t.Fatalf("confirm=%+v err=%v", job, err)
				}
			default:
				if !errors.Is(err, domain.ErrRetentionPreviewInvalid) {
					t.Fatalf("stale err=%v", err)
				}
			}
			assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE request_id='target'`, 1)
		})
	}
}

func TestConfirmedRetentionQueuedAndCompletedSurviveReopen(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	path := filepath.Join(t.TempDir(), "persisted-jobs.db")
	clock := func() time.Time { return now }
	store := openTestStore(t, path, clock)
	preview := previewConfirmedRetention(t, store, "usage_details", now)
	closeTestStore(t, store)
	store = openTestStore(t, path, clock)
	completed, err := store.ConfirmRetentionJob(ctx, preview.ID, preview.Operation, preview.ActorRef)
	if err != nil || completed.Status != "completed" {
		t.Fatalf("reopened confirmation=%+v err=%v", completed, err)
	}
	closeTestStore(t, store)
	now = now.Add(time.Hour)
	store = openTestStore(t, path, clock)
	defer closeTestStore(t, store)
	replayed, err := store.ConfirmRetentionJob(ctx, preview.ID, preview.Operation, preview.ActorRef)
	if err != nil || !reflect.DeepEqual(completed, replayed) {
		t.Fatalf("reopened result=%+v err=%v", replayed, err)
	}
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM audit_events WHERE target_ref=?`, 1, preview.ID)
}

func TestConfirmedRetentionUsageCutoffExcludesBoundary(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	store, fixture, _ := confirmedRetentionFixture(t, &now)
	cutoff := now.Add(-90 * 24 * time.Hour)
	seedRetentionRequest(t, ctx, store, fixture, "older", cutoff.Add(-time.Hour), timePointer(cutoff.Add(-time.Microsecond)))
	seedRetentionRequest(t, ctx, store, fixture, "boundary", cutoff.Add(-time.Hour), timePointer(cutoff))
	preview := previewConfirmedRetention(t, store, "usage_details", cutoff)
	if preview.ExpectedCount != 1 {
		t.Fatalf("preview cutoff count=%d", preview.ExpectedCount)
	}
	if _, err := store.ConfirmRetentionJob(ctx, preview.ID, preview.Operation, preview.ActorRef); err != nil {
		t.Fatal(err)
	}
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE request_id='boundary'`, 1)
	assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE request_id='older'`, 0)
}
