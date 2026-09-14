package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestCleanupRetentionHonorsCutoffsAndDependencies(t *testing.T) {
	ctx := context.Background()
	factCutoff := time.Date(2026, time.June, 1, 12, 0, 0, 0, time.UTC)
	auditCutoff := factCutoff.Add(-24 * time.Hour)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time {
		return factCutoff.Add(24 * time.Hour)
	})
	defer closeTestStore(t, store)
	fixture := newRetentionFixture(t, ctx, store)

	seedRetentionRequest(t, ctx, store, fixture, "expired-request",
		factCutoff.Add(-3*time.Hour), timePointer(factCutoff.Add(-2*time.Hour)), factCutoff.Add(-time.Hour))
	seedRetentionRequest(t, ctx, store, fixture, "boundary-request",
		factCutoff.Add(-time.Hour), timePointer(factCutoff), factCutoff)
	seedRetentionRequest(t, ctx, store, fixture, "retained-usage-request",
		factCutoff.Add(-3*time.Hour), timePointer(factCutoff.Add(-2*time.Hour)), factCutoff.Add(time.Hour))
	seedRetentionRequest(t, ctx, store, fixture, "in-progress-request",
		factCutoff.Add(-3*time.Hour), nil)
	seedRetentionRequest(t, ctx, store, fixture, "recent-request-old-usage",
		factCutoff.Add(-2*time.Hour), timePointer(factCutoff.Add(time.Hour)), factCutoff.Add(-time.Hour))
	seedRetentionRequest(t, ctx, store, fixture, "old-request-without-usage",
		factCutoff.Add(-3*time.Hour), timePointer(factCutoff.Add(-time.Hour)))

	seedRetentionAudit(t, ctx, store, "expired-audit", auditCutoff.Add(-time.Microsecond))
	seedRetentionAudit(t, ctx, store, "boundary-audit", auditCutoff)
	seedRetentionAudit(t, ctx, store, "recent-audit", auditCutoff.Add(time.Microsecond))

	result, errCleanup := store.CleanupRetention(ctx, RetentionCleanup{
		UsageCutoff: factCutoff,
		AuditCutoff: auditCutoff,
		BatchSize:   100,
	})
	if errCleanup != nil {
		t.Fatalf("CleanupRetention() error = %v", errCleanup)
	}
	want := RetentionCleanupResult{
		UsageEventsDeleted:   2,
		ProxyRequestsDeleted: 2,
		AuditEventsDeleted:   1,
	}
	if result != want {
		t.Fatalf("CleanupRetention() = %#v, want %#v", result, want)
	}

	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_requests WHERE request_id = ?", 0, "expired-request")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_requests WHERE request_id = ?", 0, "old-request-without-usage")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_request_auth_scopes WHERE request_id IN (?, ?)", 0,
		"expired-request", "old-request-without-usage")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_requests WHERE request_id = ?", 1, "boundary-request")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM usage_events WHERE event_id = ?", 1, "boundary-request-usage-0")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_requests WHERE request_id = ?", 1, "retained-usage-request")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM usage_events WHERE event_id = ?", 1, "retained-usage-request-usage-0")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_requests WHERE request_id = ? AND outcome = 'in_progress'", 1,
		"in-progress-request")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_requests WHERE request_id = ?", 1, "recent-request-old-usage")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM usage_events WHERE request_id = ?", 0, "recent-request-old-usage")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM audit_events WHERE id = ?", 0, "expired-audit")
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM audit_events WHERE id IN (?, ?)", 2,
		"boundary-audit", "recent-audit")
}

func TestCleanupRetentionProcessesDeterministicBatches(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), nil)
	defer closeTestStore(t, store)
	fixture := newRetentionFixture(t, ctx, store)

	for index, age := range []time.Duration{6 * time.Hour, 4 * time.Hour, 2 * time.Hour} {
		requestID := fmt.Sprintf("request-%d", index+1)
		seedRetentionRequest(t, ctx, store, fixture, requestID,
			cutoff.Add(-age-time.Hour), timePointer(cutoff.Add(-age)), cutoff.Add(-age-time.Minute))
		seedRetentionAudit(t, ctx, store, fmt.Sprintf("audit-%d", index+1), cutoff.Add(-age))
	}

	cleanup := RetentionCleanup{UsageCutoff: cutoff, AuditCutoff: cutoff, BatchSize: 1}
	for iteration := 1; iteration <= 3; iteration++ {
		result, errCleanup := store.CleanupRetention(ctx, cleanup)
		if errCleanup != nil {
			t.Fatalf("CleanupRetention(iteration %d) error = %v", iteration, errCleanup)
		}
		want := RetentionCleanupResult{UsageEventsDeleted: 1, ProxyRequestsDeleted: 1, AuditEventsDeleted: 1}
		if result != want {
			t.Fatalf("CleanupRetention(iteration %d) = %#v, want %#v", iteration, result, want)
		}
		assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_requests WHERE request_id = ?", 0,
			fmt.Sprintf("request-%d", iteration))
		assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM audit_events WHERE id = ?", 0,
			fmt.Sprintf("audit-%d", iteration))
		if iteration < 3 {
			assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_requests WHERE request_id = ?", 1,
				fmt.Sprintf("request-%d", iteration+1))
			assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM audit_events WHERE id = ?", 1,
				fmt.Sprintf("audit-%d", iteration+1))
		}
	}

	result, errCleanup := store.CleanupRetention(ctx, cleanup)
	if errCleanup != nil {
		t.Fatalf("CleanupRetention(empty) error = %v", errCleanup)
	}
	if result != (RetentionCleanupResult{}) {
		t.Fatalf("CleanupRetention(empty) = %#v, want zero result", result)
	}
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_request_auth_scopes", 0)
}

func TestExecuteRetentionClosedPeriodsDetachesRequestReferences(t *testing.T) {
	ctx := context.Background()
	cutoff := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), func() time.Time {
		return cutoff.Add(24 * time.Hour)
	})
	defer closeTestStore(t, store)
	fixture := newRetentionFixture(t, ctx, store)
	period, errPeriod := store.EnsureBillingPeriod(ctx, domain.BillingPeriod{
		MembershipID: fixture.membership.ID, MemberRefSnapshot: fixture.membership.MemberRef,
		CarID: fixture.car.ID, Timezone: "UTC", AnchorAt: cutoff.Add(-72 * time.Hour),
		From: cutoff.Add(-48 * time.Hour), To: cutoff.Add(-24 * time.Hour),
	})
	if errPeriod != nil {
		t.Fatalf("EnsureBillingPeriod() error = %v", errPeriod)
	}
	seedRetentionRequest(t, ctx, store, fixture, "closed-period-request",
		cutoff.Add(-36*time.Hour), timePointer(cutoff.Add(-30*time.Hour)), cutoff.Add(-32*time.Hour))
	if _, errUpdate := store.db.ExecContext(ctx, "UPDATE proxy_requests SET billing_period_id = ? WHERE request_id = ?", period.ID, "closed-period-request"); errUpdate != nil {
		t.Fatalf("attach request to billing period: %v", errUpdate)
	}

	deleted, inFlight, errExecute := store.ExecuteRetention(ctx, "closed_periods", cutoff, 10)
	if errExecute != nil {
		t.Fatalf("ExecuteRetention(closed_periods) error = %v", errExecute)
	}
	if deleted != 1 || inFlight != 0 {
		t.Fatalf("ExecuteRetention(closed_periods) = (%d, %d), want (1, 0)", deleted, inFlight)
	}
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM billing_periods WHERE id = ?", 0, period.ID)
	assertRetentionRowCount(t, store, "SELECT COUNT(*) FROM proxy_requests WHERE request_id = ? AND billing_period_id IS NULL", 1, "closed-period-request")
}

func TestCleanupRetentionValidatesInputAndContext(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), nil)
	defer closeTestStore(t, store)
	cutoff := time.Date(2026, time.August, 1, 12, 0, 0, 0, time.UTC)

	invalidInputs := []RetentionCleanup{
		{AuditCutoff: cutoff, BatchSize: 1},
		{UsageCutoff: cutoff, BatchSize: 1},
		{UsageCutoff: cutoff, AuditCutoff: cutoff},
	}
	for _, input := range invalidInputs {
		if _, errCleanup := store.CleanupRetention(context.Background(), input); !errors.Is(errCleanup, domain.ErrInvalid) {
			t.Fatalf("CleanupRetention(%#v) error = %v, want ErrInvalid", input, errCleanup)
		}
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, errCleanup := store.CleanupRetention(canceled, RetentionCleanup{
		UsageCutoff: cutoff,
		AuditCutoff: cutoff,
		BatchSize:   1,
	})
	if !errors.Is(errCleanup, context.Canceled) {
		t.Fatalf("CleanupRetention(canceled) error = %v, want context.Canceled", errCleanup)
	}
}

func TestRetentionMigrationProvidesCascadeAndCleanupIndexes(t *testing.T) {
	store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), nil)
	defer closeTestStore(t, store)

	assertRetentionRowCount(t, store, `
		SELECT COUNT(*)
		FROM sqlite_master
		WHERE type = 'index'
		  AND name IN (
			'proxy_requests_terminal_completed',
			'usage_events_requested',
			'audit_events_occurred'
		  )
	`, 3)

	rows, errQuery := store.db.QueryContext(context.Background(), "PRAGMA foreign_key_list(proxy_request_auth_scopes)")
	if errQuery != nil {
		t.Fatalf("query request scope foreign keys: %v", errQuery)
	}
	defer func() {
		if errClose := rows.Close(); errClose != nil {
			t.Errorf("close request scope foreign keys: %v", errClose)
		}
	}()
	foundCascade := false
	for rows.Next() {
		var id, sequence int
		var table, from, to, onUpdate, onDelete, match string
		if errScan := rows.Scan(&id, &sequence, &table, &from, &to, &onUpdate, &onDelete, &match); errScan != nil {
			t.Fatalf("scan request scope foreign key: %v", errScan)
		}
		if table == "proxy_requests" && from == "request_id" && to == "request_id" && onDelete == "CASCADE" {
			foundCascade = true
		}
	}
	if errRows := rows.Err(); errRows != nil {
		t.Fatalf("iterate request scope foreign keys: %v", errRows)
	}
	if !foundCascade {
		t.Fatal("proxy_request_auth_scopes request_id foreign key does not use ON DELETE CASCADE")
	}
}

type retentionFixture struct {
	user       domain.User
	apiKey     domain.APIKey
	car        domain.Car
	membership domain.Membership
	assignment domain.AuthAssignment
}

func newRetentionFixture(t *testing.T, ctx context.Context, store *Store) retentionFixture {
	t.Helper()
	admin, errAdmin := store.BootstrapAdmin(ctx, testUser("retention-admin", domain.UserRoleAdmin), nil)
	if errAdmin != nil {
		t.Fatalf("BootstrapAdmin() error = %v", errAdmin)
	}
	user, errUser := store.CreateUser(ctx, testUser("retention-passenger", domain.UserRolePassenger))
	if errUser != nil {
		t.Fatalf("CreateUser() error = %v", errUser)
	}
	apiKey, errKey := store.CreateAPIKey(ctx, domain.APIKey{
		UserID: user.ID, Name: "retention", SecretDigest: []byte("retention-digest"),
	})
	if errKey != nil {
		t.Fatalf("CreateAPIKey() error = %v", errKey)
	}
	car, errCar := store.CreateCar(ctx, domain.Car{Name: "retention-car", Status: domain.CarStatusActive})
	if errCar != nil {
		t.Fatalf("CreateCar() error = %v", errCar)
	}
	membership, errMembership := store.MoveMembership(ctx, domain.MembershipMove{Membership: domain.Membership{
		UserID: user.ID, CarID: car.ID, DisplayName: "Retention Passenger", CreatedByUserID: admin.ID,
	}})
	if errMembership != nil {
		t.Fatalf("MoveMembership() error = %v", errMembership)
	}
	assignment, errAssignment := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{
		CarID: car.ID, AuthID: "retention-auth", SafeLabel: "Retention Account",
		ProviderSnapshot: "codex", CreatedByUserID: admin.ID,
	}})
	if errAssignment != nil {
		t.Fatalf("MoveAuthAssignment() error = %v", errAssignment)
	}
	return retentionFixture{user: user, apiKey: apiKey, car: car, membership: membership, assignment: assignment}
}

func seedRetentionRequest(
	t *testing.T,
	ctx context.Context,
	store *Store,
	fixture retentionFixture,
	requestID string,
	startedAt time.Time,
	completedAt *time.Time,
	usageTimes ...time.Time,
) {
	t.Helper()
	_, errBegin := store.BeginProxyRequest(ctx, domain.ProxyRequest{
		RequestID: requestID, UserID: fixture.user.ID, APIKeyID: fixture.apiKey.KeyID,
		CarID: fixture.car.ID, MembershipID: fixture.membership.ID,
		MemberRefSnapshot: fixture.membership.MemberRef, DisplayNameSnapshot: fixture.membership.DisplayName,
		ScopeHash: "retention-scope", ScopeSize: 1, SourceFormat: "openai", RequestedModel: "retention-model",
		Outcome: domain.RequestOutcomeInProgress, StartedAt: startedAt,
	}, []domain.ProxyRequestAuthScope{{
		AuthID: fixture.assignment.AuthID, AssignmentID: fixture.assignment.ID,
		AccountRefSnapshot: fixture.assignment.AccountRef, SafeLabelSnapshot: fixture.assignment.SafeLabel,
		ProviderSnapshot: fixture.assignment.ProviderSnapshot,
	}})
	if errBegin != nil {
		t.Fatalf("BeginProxyRequest(%q) error = %v", requestID, errBegin)
	}
	for index, requestedAt := range usageTimes {
		_, errUsage := store.InsertUsageEvent(ctx, domain.UsageEvent{
			EventID: fmt.Sprintf("%s-usage-%d", requestID, index), RequestID: requestID,
			AuthID: fixture.assignment.AuthID, Provider: fixture.assignment.ProviderSnapshot,
			Model: "retention-model", UsageKnown: false, RequestedAt: requestedAt,
		})
		if errUsage != nil {
			t.Fatalf("InsertUsageEvent(%q, %d) error = %v", requestID, index, errUsage)
		}
	}
	if completedAt == nil {
		return
	}
	if errComplete := store.CompleteProxyRequest(ctx, domain.RequestCompletion{
		RequestID: requestID, Outcome: domain.RequestOutcomeSucceeded, CompletedAt: *completedAt,
		StatusClass: "2xx", UpstreamAttempted: len(usageTimes) > 0,
	}); errComplete != nil {
		t.Fatalf("CompleteProxyRequest(%q) error = %v", requestID, errComplete)
	}
}

func seedRetentionAudit(t *testing.T, ctx context.Context, store *Store, id string, occurredAt time.Time) {
	t.Helper()
	_, errAudit := store.InsertAuditEvent(ctx, domain.AuditEvent{
		ID: id, OccurredAt: occurredAt, ActorType: "system", ActorRef: "retention",
		Action: "retention.fixture", TargetType: "test", TargetRef: id, Result: "success",
	})
	if errAudit != nil {
		t.Fatalf("InsertAuditEvent(%q) error = %v", id, errAudit)
	}
}

func assertRetentionRowCount(t *testing.T, store *Store, query string, want int, args ...any) {
	t.Helper()
	var got int
	if errQuery := store.db.QueryRowContext(context.Background(), query, args...).Scan(&got); errQuery != nil {
		t.Fatalf("query retention row count: %v", errQuery)
	}
	if got != want {
		t.Fatalf("retention row count = %d, want %d for %q", got, want, query)
	}
}

func timePointer(value time.Time) *time.Time {
	return &value
}

func TestRetentionProtectsInterruptedFactsAndPeriods(t *testing.T) {
	for _, operation := range []string{"automatic", "usage_details", "closed_periods"} {
		t.Run(operation, func(t *testing.T) {
			ctx := context.Background()
			cutoff := time.Date(2026, time.July, 1, 0, 0, 0, 0, time.UTC)
			store := openTestStore(t, filepath.Join(t.TempDir(), "carpool.db"), nil)
			defer closeTestStore(t, store)
			fixture := newRetentionFixture(t, ctx, store)
			for i, outcome := range []string{"in_progress", "incomplete", "succeeded"} {
				from := cutoff.Add(-time.Duration(10-i*2) * 24 * time.Hour)
				period, err := store.EnsureBillingPeriod(ctx, domain.BillingPeriod{
					MembershipID: fixture.membership.ID, MemberRefSnapshot: fixture.membership.MemberRef,
					CarID: fixture.car.ID, Timezone: "UTC", AnchorAt: from, From: from, To: from.Add(24 * time.Hour),
				})
				if err != nil {
					t.Fatal(err)
				}
				seedRetentionRequest(t, ctx, store, fixture, outcome, from, timePointer(from.Add(time.Hour)), from)
				if _, err = store.db.ExecContext(ctx, `UPDATE proxy_requests SET outcome=?, completed_at=CASE WHEN ?='in_progress' THEN NULL ELSE completed_at END, billing_period_id=? WHERE request_id=?`, outcome, outcome, period.ID, outcome); err != nil {
					t.Fatal(err)
				}
				if _, err = store.db.ExecContext(ctx, `UPDATE usage_events SET billing_period_id=? WHERE request_id=?`, period.ID, outcome); err != nil {
					t.Fatal(err)
				}
			}
			if operation == "automatic" {
				result, err := store.CleanupRetention(ctx, RetentionCleanup{UsageCutoff: cutoff, AuditCutoff: cutoff, BatchSize: 100})
				if err != nil || result.ProxyRequestsDeleted != 1 || result.UsageEventsDeleted != 1 {
					t.Fatalf("cleanup = %#v, %v", result, err)
				}
			} else {
				preview, _, err := store.PreviewRetention(ctx, operation, cutoff)
				if err != nil || preview != 1 {
					t.Fatalf("preview = %d, %v", preview, err)
				}
				deleted, _, err := store.ExecuteRetention(ctx, operation, cutoff, 100)
				if err != nil || deleted != preview {
					t.Fatalf("execute = %d, %v", deleted, err)
				}
			}
			for _, outcome := range []string{"in_progress", "incomplete"} {
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE request_id=? AND outcome=? AND billing_period_id IS NOT NULL`, 1, outcome, outcome)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM usage_events WHERE request_id=? AND billing_period_id IS NOT NULL`, 1, outcome)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_request_auth_scopes WHERE request_id=?`, 1, outcome)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM billing_periods WHERE id=(SELECT billing_period_id FROM proxy_requests WHERE request_id=?)`, 1, outcome)
			}
			// Reset offsets confirmed spend and unknown totals, not raw facts or interruption markers.
			at := cutoff.Add(-10 * 24 * time.Hour).Add(time.Hour)
			if _, err := store.db.ExecContext(ctx, `UPDATE billing_periods SET unknown_cost_events=2`); err != nil {
				t.Fatal(err)
			}
			if _, _, err := store.ExecuteRetention(ctx, "reset_current_period", at, 100); err != nil {
				t.Fatal(err)
			}
			assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM billing_periods WHERE unknown_cost_events=0 AND id=(SELECT billing_period_id FROM proxy_requests WHERE request_id='in_progress')`, 1)
			assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE outcome IN ('in_progress','incomplete')`, 2)
			for _, outcome := range []string{"in_progress", "incomplete"} {
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM proxy_requests WHERE request_id=? AND outcome=? AND billing_period_id IS NOT NULL`, 1, outcome, outcome)
				assertRetentionRowCount(t, store, `SELECT COUNT(*) FROM usage_events WHERE request_id=? AND billing_period_id IS NOT NULL`, 1, outcome)
			}
		})
	}
}
