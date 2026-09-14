package sqlite

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestUsageRequestsFiltersAndLogicalCounts(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "usage.db"), func() time.Time { return now })
	defer closeTestStore(t, store)
	fixture := newRetentionFixture(t, ctx, store)
	otherKey, errKey := store.CreateAPIKey(ctx, domain.APIKey{UserID: fixture.user.ID, Name: "other", SecretDigest: []byte("other-test-digest")})
	if errKey != nil {
		t.Fatal(errKey)
	}
	otherAssignment, errAssignment := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{CarID: fixture.car.ID, AuthID: "other-auth", SafeLabel: "Other account", ProviderSnapshot: "codex", CreatedByUserID: fixture.membership.CreatedByUserID}})
	if errAssignment != nil {
		t.Fatal(errAssignment)
	}
	type row struct {
		id, requested, actual, billing string
		outcome                        domain.RequestOutcome
		key                            domain.APIKey
		assignment                     domain.AuthAssignment
		started                        time.Time
	}
	rows := []row{
		{"target", "wanted", "wanted", "unknown", domain.RequestOutcomeSucceeded, fixture.apiKey, fixture.assignment, now},
		{"actual-only", "alias", "wanted", "unknown", domain.RequestOutcomeSucceeded, fixture.apiKey, fixture.assignment, now},
		{"request-only", "wanted", "other-model", "unknown", domain.RequestOutcomeSucceeded, fixture.apiKey, fixture.assignment, now},
		{"other-model", "alias", "other-model", "unknown", domain.RequestOutcomeSucceeded, fixture.apiKey, fixture.assignment, now},
		{"other-key", "wanted", "wanted", "unknown", domain.RequestOutcomeSucceeded, otherKey, fixture.assignment, now},
		{"other-account", "wanted", "wanted", "unknown", domain.RequestOutcomeSucceeded, fixture.apiKey, otherAssignment, now},
		{"other-outcome", "wanted", "wanted", "unknown", domain.RequestOutcomeFailed, fixture.apiKey, fixture.assignment, now},
		{"other-billing", "wanted", "wanted", "priced", domain.RequestOutcomeSucceeded, fixture.apiKey, fixture.assignment, now},
		{"at-from", "wanted", "wanted", "unknown", domain.RequestOutcomeSucceeded, fixture.apiKey, fixture.assignment, now.Add(-time.Hour)},
		{"before-from", "wanted", "wanted", "unknown", domain.RequestOutcomeSucceeded, fixture.apiKey, fixture.assignment, now.Add(-time.Hour - time.Microsecond)},
		{"at-to", "wanted", "wanted", "unknown", domain.RequestOutcomeSucceeded, fixture.apiKey, fixture.assignment, now.Add(time.Hour)},
	}
	for _, r := range rows {
		beginReportRequest(t, ctx, store, r.id, r.started, fixture.user, r.key, fixture.car, fixture.membership, r.assignment)
		for i := 0; i < 3; i++ {
			model := r.actual
			if i == 2 {
				model = "nonmatching-event"
			}
			_, errEvent := store.InsertUsageEvent(ctx, domain.UsageEvent{EventID: fmt.Sprintf("%s-event-%d", r.id, i), RequestID: r.id, AuthID: r.assignment.AuthID, Provider: "codex", Model: model, RequestedAt: now.Add(2 * time.Hour), PricingStatus: "unknown"})
			if errEvent != nil {
				t.Fatal(errEvent)
			}
		}
		if errComplete := store.CompleteProxyRequest(ctx, domain.RequestCompletion{RequestID: r.id, RequestedModel: r.requested, Outcome: r.outcome, CompletedAt: r.started.Add(time.Minute)}); errComplete != nil {
			t.Fatal(errComplete)
		}
		if _, errUpdate := store.db.ExecContext(ctx, "UPDATE proxy_requests SET billing_status=? WHERE request_id=?", r.billing, r.id); errUpdate != nil {
			t.Fatal(errUpdate)
		}
	}
	base := domain.UsageRequestFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	// Exercise every subset, so both individual predicates and their AND composition matter.
	for mask := 0; mask < 256; mask++ {
		t.Run(fmt.Sprintf("filters_%08b", mask), func(t *testing.T) {
			f := base
			if mask&1 != 0 {
				f.CarID = fixture.car.ID
			}
			if mask&2 != 0 {
				f.UserID = fixture.user.ID
			}
			if mask&4 != 0 {
				f.AssignmentID = fixture.assignment.ID
			}
			if mask&8 != 0 {
				f.APIKeyID = fixture.apiKey.KeyID
			}
			if mask&16 != 0 {
				f.RequestedModel = "wanted"
			}
			if mask&32 != 0 {
				f.Outcome = "succeeded"
			}
			if mask&64 != 0 {
				f.BillingStatus = "unknown"
			}
			if mask&128 != 0 {
				f.RequestID = "target"
			}
			var want []string
			for _, r := range rows {
				if r.started.Before(f.From) || !r.started.Before(f.To) || (f.AssignmentID != "" && r.assignment.ID != f.AssignmentID) || (f.APIKeyID != "" && r.key.KeyID != f.APIKeyID) || (f.RequestedModel != "" && r.requested != f.RequestedModel && r.actual != f.RequestedModel) || (f.Outcome != "" && string(r.outcome) != f.Outcome) || (f.BillingStatus != "" && r.billing != f.BillingStatus) || (f.RequestID != "" && r.id != f.RequestID) {
					continue
				}
				want = append(want, r.id)
			}
			assertUsageRequestIDs(t, store, f, want)
		})
	}
	for _, field := range []string{"car", "user", "assignment", "key", "model", "outcome", "billing", "request", "request-prefix", "request-sql"} {
		t.Run("nonmatching_"+field, func(t *testing.T) {
			f := base
			switch field {
			case "car":
				f.CarID = "missing"
			case "user":
				f.UserID = "missing"
			case "assignment":
				f.AssignmentID = "missing"
			case "key":
				f.APIKeyID = "missing"
			case "model":
				f.RequestedModel = "want"
			case "outcome":
				f.Outcome = "rejected"
			case "billing":
				f.BillingStatus = "legacy_unpriced"
			case "request":
				f.RequestID = "Target"
			case "request-prefix":
				f.RequestID = "targ"
			case "request-sql":
				f.RequestID = "' OR 1=1 --"
			}
			assertUsageRequestIDs(t, store, f, nil)
		})
	}
	detail, errDetail := store.GetUsageRequestDetail(ctx, "actual-only")
	if errDetail != nil {
		t.Fatal(errDetail)
	}
	if detail.EventCount != 3 || detail.UnknownEvents != 3 || len(detail.Events) != 3 || detail.Events[2].Model != "nonmatching-event" {
		t.Fatalf("filtered expansion lost events: %#v", detail)
	}
	if detail.Request.BilledNanoUSD != nil || detail.Events[0].TotalTokens != nil || detail.Events[0].CostNanoUSD != nil || detail.Events[0].AttemptNo != nil || detail.Events[0].EventSeq != nil {
		t.Fatalf("unknown values or attempt numbers fabricated: %#v", detail)
	}
}

func assertUsageRequestIDs(t *testing.T, store *Store, filter domain.UsageRequestFilter, want []string) {
	t.Helper()
	items, errList := store.ListUsageRequests(context.Background(), filter, time.Time{}, "", 100)
	if errList != nil {
		t.Fatal(errList)
	}
	got := make([]string, 0, len(items))
	for _, item := range items {
		got = append(got, item.Request.RequestID)
		if item.EventCount != 3 || len(item.Events) != 0 {
			t.Fatalf("list must count all events without expanding: %#v", item)
		}
	}
	slices.Sort(got)
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("request IDs = %v, want %v", got, want)
	}
	count, errCount := store.CountUsageRequests(context.Background(), filter)
	if errCount != nil || count != int64(len(want)) {
		t.Fatalf("CountUsageRequests = %d, %v; want %d logical requests", count, errCount, len(want))
	}
}

func TestUsageRequestsStableTuplePages(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 14, 12, 0, 0, 123456000, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "usage.db"), func() time.Time { return now })
	defer closeTestStore(t, store)
	fixture := newRetentionFixture(t, ctx, store)
	for i := 0; i < 55; i++ {
		seedRetentionRequest(t, ctx, store, fixture, fmt.Sprintf("request-%02d", i), now, &now)
	}
	f := domain.UsageRequestFilter{From: now.Add(-time.Hour), To: now.Add(time.Hour)}
	var before time.Time
	var beforeID string
	var got []string
	for page := 0; page < 3; page++ {
		items, errList := store.ListUsageRequests(ctx, f, before, beforeID, 25)
		wantSize := 25
		if page == 2 {
			wantSize = 5
		}
		if errList != nil || len(items) != wantSize {
			t.Fatalf("page %d = %d rows, %v; want %d", page, len(items), errList, wantSize)
		}
		for _, item := range items {
			got = append(got, item.Request.RequestID)
		}
		last := items[len(items)-1].Request
		before, beforeID = last.StartedAt, last.RequestID
		if page == 0 {
			seedRetentionRequest(t, ctx, store, fixture, "request-99", now, &now)
			later := now.Add(time.Minute)
			seedRetentionRequest(t, ctx, store, fixture, "newer", later, &later)
		}
	}
	for i, id := range got {
		if want := fmt.Sprintf("request-%02d", 54-i); id != want {
			t.Fatalf("row %d = %q, want %q", i, id, want)
		}
	}
	tail, errTail := store.ListUsageRequests(ctx, f, before, beforeID, 25)
	if errTail != nil || len(tail) != 0 {
		t.Fatalf("tail = %v, %v", tail, errTail)
	}
	count, errCount := store.CountUsageRequests(ctx, f)
	if errCount != nil || count != 57 {
		t.Fatalf("live count = %d, %v; want 57", count, errCount)
	}
}

func TestUsageRequestsValidationAndMissingDetail(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "usage.db"), nil)
	defer closeTestStore(t, store)
	for _, f := range []domain.UsageRequestFilter{{}, {From: now}, {To: now}, {From: now, To: now}, {From: now, To: now.Add(-time.Hour)}} {
		if _, errList := store.ListUsageRequests(ctx, f, time.Time{}, "", 25); !errors.Is(errList, domain.ErrInvalid) {
			t.Fatalf("invalid filter list: %v", errList)
		}
		if _, errCount := store.CountUsageRequests(ctx, f); !errors.Is(errCount, domain.ErrInvalid) {
			t.Fatalf("invalid filter count: %v", errCount)
		}
	}
	f := domain.UsageRequestFilter{From: now, To: now.Add(time.Hour)}
	for _, limit := range []int{0, -1, 101} {
		if _, errList := store.ListUsageRequests(ctx, f, time.Time{}, "", limit); !errors.Is(errList, domain.ErrInvalid) {
			t.Fatalf("invalid limit %d: %v", limit, errList)
		}
	}
	if _, errDetail := store.GetUsageRequestDetail(ctx, " "); !errors.Is(errDetail, domain.ErrInvalid) {
		t.Fatalf("blank detail ID: %v", errDetail)
	}
	if _, errDetail := store.GetUsageRequestDetail(ctx, "missing"); !errors.Is(errDetail, domain.ErrNotFound) {
		t.Fatalf("missing detail: %v", errDetail)
	}
}

func TestUsageRequestsExpandAllAttemptsAndPreserveSnapshots(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, time.September, 14, 12, 0, 0, 0, time.UTC)
	store := openTestStore(t, filepath.Join(t.TempDir(), "usage.db"), func() time.Time { return now })
	defer closeTestStore(t, store)
	fixture := newRetentionFixture(t, ctx, store)
	second, errAssignment := store.MoveAuthAssignment(ctx, domain.AuthAssignmentMove{AuthAssignment: domain.AuthAssignment{CarID: fixture.car.ID, AuthID: "second-auth", SafeLabel: "Second Account", ProviderSnapshot: "codex", CreatedByUserID: fixture.membership.CreatedByUserID}})
	if errAssignment != nil {
		t.Fatal(errAssignment)
	}
	scopes := []domain.ProxyRequestAuthScope{
		{AuthID: fixture.assignment.AuthID, AssignmentID: fixture.assignment.ID, AccountRefSnapshot: fixture.assignment.AccountRef, SafeLabelSnapshot: fixture.assignment.SafeLabel, ProviderSnapshot: "codex"},
		{AuthID: second.AuthID, AssignmentID: second.ID, AccountRefSnapshot: second.AccountRef, SafeLabelSnapshot: second.SafeLabel, ProviderSnapshot: "codex"},
	}
	for _, id := range []string{"attempts", "scope-only"} {
		_, errBegin := store.BeginProxyRequest(ctx, domain.ProxyRequest{RequestID: id, UserID: fixture.user.ID, APIKeyID: fixture.apiKey.KeyID, CarID: fixture.car.ID, MembershipID: fixture.membership.ID, MemberRefSnapshot: fixture.membership.MemberRef, DisplayNameSnapshot: fixture.membership.DisplayName, ScopeHash: "test-scope", ScopeSize: 2, SourceFormat: "openai", RequestedModel: "alias", StartedAt: now, Outcome: domain.RequestOutcomeInProgress}, scopes)
		if errBegin != nil {
			t.Fatal(errBegin)
		}
	}
	zero, tokens, cost := int64(0), int64(225), int64(225_000_000)
	events := []domain.UsageEvent{
		{EventID: "event-c", AuthID: second.AuthID, Model: "actual", PricingStatus: "legacy_unpriced"},
		{EventID: "event-b", AuthID: second.AuthID, Model: "actual", PricingStatus: "unknown"},
		{EventID: "event-a", AuthID: fixture.assignment.AuthID, Model: "other", PricingStatus: "priced", UsageKnown: true, InputTokens: &tokens, OutputTokens: &zero, TotalTokens: &tokens, PriceInputPerToken: "0.001", CostNanoUSD: &cost, Failed: true, StatusClass: "5xx"},
	}
	for _, event := range events {
		event.RequestID, event.Provider, event.RequestedAt = "attempts", "codex", now
		if _, errInsert := store.InsertUsageEvent(ctx, event); errInsert != nil {
			t.Fatal(errInsert)
		}
	}
	if _, errUpdate := store.db.ExecContext(ctx, "UPDATE proxy_requests SET billing_status='unknown', billed_nano_usd=? WHERE request_id='attempts'", cost); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, errUpdate := store.db.ExecContext(ctx, "UPDATE memberships SET display_name='Renamed Passenger' WHERE id=?", fixture.membership.ID); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if _, errUpdate := store.db.ExecContext(ctx, "UPDATE car_auth_assignments SET safe_label='Renamed Account' WHERE id=?", fixture.assignment.ID); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	f := domain.UsageRequestFilter{From: now.Add(-time.Minute), To: now.Add(time.Minute), AssignmentID: fixture.assignment.ID, RequestedModel: "actual"}
	assertUsageRequestIDs(t, store, f, []string{"attempts"})
	detail, errDetail := store.GetUsageRequestDetail(ctx, "attempts")
	if errDetail != nil {
		t.Fatal(errDetail)
	}
	if len(detail.Events) != 3 || detail.EventCount != 3 || detail.UnknownEvents != 1 || detail.Request.BillingStatus != "unknown" || detail.Request.BilledNanoUSD == nil || *detail.Request.BilledNanoUSD != cost {
		t.Fatalf("incomplete confirmed subtotal = %#v", detail)
	}
	if detail.Request.DisplayNameSnapshot != fixture.membership.DisplayName || detail.Request.MemberRefSnapshot != fixture.membership.MemberRef || detail.UserRef != fixture.user.UserRef || detail.CarRef != fixture.car.CarRef || detail.APIKeyRef != fixture.apiKey.KeyID {
		t.Fatalf("request identity snapshot changed: %#v", detail)
	}
	for i, event := range detail.Events {
		if event.EventID != fmt.Sprintf("event-%c", 'a'+i) || event.EventSeq != nil || event.AttemptNo != nil {
			t.Fatalf("unstable/invented event order: %#v", detail.Events)
		}
	}
	priced := detail.Events[0]
	if !priced.UsageKnown || priced.TotalTokens == nil || *priced.TotalTokens != tokens || priced.OutputTokens == nil || *priced.OutputTokens != 0 || priced.CostNanoUSD == nil || *priced.CostNanoUSD != cost || !priced.Failed || priced.SafeLabelSnapshot != fixture.assignment.SafeLabel || priced.AccountRefSnapshot != fixture.assignment.AccountRef {
		t.Fatalf("priced failed event snapshot = %#v", priced)
	}
	for _, event := range detail.Events[1:] {
		if event.UsageKnown || event.TotalTokens != nil || event.CostNanoUSD != nil {
			t.Fatalf("unknown or legacy event fabricated zero: %#v", event)
		}
	}
	if detail.Events[2].PricingStatus != "legacy_unpriced" {
		t.Fatalf("legacy status changed: %#v", detail.Events[2])
	}
}
