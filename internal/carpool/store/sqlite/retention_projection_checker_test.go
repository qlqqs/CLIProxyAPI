package sqlite

import (
	"reflect"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestCheckerAutomaticEventBatchPreservesUnknownBillingStatusAndLedger(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC)
	store, fixture, period := confirmedRetentionFixture(t, &now)
	started := now.Add(-92 * 24 * time.Hour)
	completed := now.Add(-91 * 24 * time.Hour)
	seedRetentionRequest(t, t.Context(), store, fixture, "checker-partial", started, &completed)
	unknown := domain.UsageEvent{EventID: "checker-unknown", RequestID: "checker-partial", AuthID: fixture.assignment.AuthID, Provider: "codex", Model: "test", RequestedAt: started}
	if _, errEvent := store.RecordUsageEventBilled(t.Context(), unknown, period.ID, nil, "unknown", "missing_usage"); errEvent != nil {
		t.Fatal(errEvent)
	}
	known := unknown
	known.EventID = "checker-known"
	known.UsageKnown = true
	known.RequestedAt = started.Add(time.Hour)
	cost := int64(225_000_000)
	if _, errEvent := store.RecordUsageEventBilled(t.Context(), known, period.ID, &cost, "priced", ""); errEvent != nil {
		t.Fatal(errEvent)
	}
	before, errBefore := store.GetBillingPeriod(t.Context(), fixture.membership.ID, now)
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	result, errCleanup := store.CleanupRetention(t.Context(), RetentionCleanup{UsageCutoff: now.Add(-90 * 24 * time.Hour), AuditCutoff: now.Add(-180 * 24 * time.Hour), BatchSize: 1})
	if errCleanup != nil || result.UsageEventsDeleted != 1 || result.ProxyRequestsDeleted != 0 {
		t.Fatalf("automatic batch = %+v, %v", result, errCleanup)
	}
	detail, errDetail := store.GetUsageRequestDetail(t.Context(), "checker-partial")
	if errDetail != nil || detail.UnknownEvents != 0 || detail.EventCount != 1 || detail.Request.BillingStatus != "unknown" || detail.Request.BilledNanoUSD == nil || *detail.Request.BilledNanoUSD != cost {
		t.Fatalf("partial projection = %+v, %v", detail, errDetail)
	}
	after, errAfter := store.GetBillingPeriod(t.Context(), fixture.membership.ID, now)
	if errAfter != nil || !reflect.DeepEqual(before, after) {
		t.Fatalf("automatic event pruning changed ledger: before=%+v after=%+v error=%v", before, after, errAfter)
	}
}
