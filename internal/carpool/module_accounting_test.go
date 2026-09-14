package carpool

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	carpoolaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/access"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/accounting"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolruntime "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/runtime"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func accountingRequest(t *testing.T, f *serverRouteFixture) *http.Request {
	t.Helper()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	result := &sdkaccess.Result{Provider: carpoolaccess.ProviderName, Principal: "test", Metadata: map[string]string{carpoolaccess.MetadataUserID: f.passenger.ID, carpoolaccess.MetadataAPIKeyID: f.apiKeyID}}
	if err := f.module.AuthenticatedRequestHook(request.Context(), request, result); err != nil {
		t.Fatal(err)
	}
	return request
}
func accountingRecord() usage.Record {
	return usage.Record{EventID: "sync-event", AuthID: "route-a-openai-1", Provider: "openai", Model: "gpt-5.4", UsageKnown: true, Detail: usage.Detail{InputTokens: 100, OutputTokens: 20, TotalTokens: 120}}
}

func TestModulePublishPersistsBeforeReturnAndDeduplicates(t *testing.T) {
	f := newServerRouteFixture(t, false)
	request := accountingRequest(t, f)
	snapshot, _ := carpoolruntime.AuthorizationFromContext(request.Context())
	manager := usage.NewManager(1)
	manager.Register(f.module.writer)
	ctx, cancel := context.WithCancel(request.Context())
	cancel()
	record := accountingRecord()
	record.RequestID = "untrusted-request-id"
	manager.Publish(ctx, record)
	first, err := f.module.store.GetProxyRequest(context.Background(), snapshot.RequestID())
	if err != nil {
		t.Fatal(err)
	}
	if first.BilledNanoUSD == nil || *first.BilledNanoUSD <= 0 {
		t.Fatalf("Publish returned without confirmed amount: %#v", first)
	}
	manager.Publish(ctx, record)
	manager.Stop()
	again, err := f.module.store.GetProxyRequest(context.Background(), snapshot.RequestID())
	if err != nil {
		t.Fatal(err)
	}
	if again.BilledNanoUSD == nil || *again.BilledNanoUSD != *first.BilledNanoUSD {
		t.Fatal("duplicate charged twice")
	}
	detail, err := f.module.store.GetUsageRequestDetail(context.Background(), snapshot.RequestID())
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.Events) != 1 {
		t.Fatalf("events = %d", len(detail.Events))
	}
}

func TestModuleAccountingFailureHTTPGateRecoveryAndClose(t *testing.T) {
	f := newServerRouteFixture(t, false, true)
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.databasePath))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := raw.Close(); errClose != nil {
			t.Error(errClose)
		}
	}()
	exec := func(query string) {
		t.Helper()
		if _, errExec := raw.Exec(query); errExec != nil {
			t.Fatal(errExec)
		}
	}
	request := accountingRequest(t, f)
	snapshot, _ := carpoolruntime.AuthorizationFromContext(request.Context())
	exec(`CREATE TRIGGER fail_accounting BEFORE INSERT ON usage_events BEGIN SELECT RAISE(ABORT, 'sensitive-db-failure'); END`)
	manager := usage.NewManager(1)
	manager.Register(f.module.writer)
	manager.Publish(request.Context(), accountingRecord())
	manager.Stop()
	f.module.writer.HandleRequestCompletion(context.Background(), pluginapi.RequestCompletion{RequestID: snapshot.RequestID(), Outcome: pluginapi.RequestCompletionSucceeded, CompletedAt: time.Now()})
	pending, err := f.module.store.GetProxyRequest(context.Background(), snapshot.RequestID())
	if err != nil {
		t.Fatal(err)
	}
	if pending.Outcome != domain.RequestOutcomeInProgress || f.module.writer.Snapshot().PendingRecords != 2 {
		t.Fatal("failed event or restart barrier lost")
	}
	selectedBefore, _ := f.recorder.snapshot()
	response := f.request(t, http.MethodPost, "/v1/chat/completions", `{"model":"`+serverRouteOpenAIModel+`","messages":[{"role":"user","content":"test"}]}`, false)
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if errDecode := json.Unmarshal(response.Body.Bytes(), &body); errDecode != nil {
		t.Fatal(errDecode)
	}
	if response.Code != 503 || body.Error.Code != "accounting_unavailable" {
		t.Fatalf("response %d %s", response.Code, response.Body.String())
	}
	selectedAfter, _ := f.recorder.snapshot()
	if len(selectedAfter) != len(selectedBefore) {
		t.Fatal("unavailable accounting reached upstream")
	}
	for i := 0; i < 16; i++ {
		models := f.request(t, http.MethodGet, "/v1/models", "", false)
		if models.Code != 200 {
			t.Fatalf("model query blocked: %d %s", models.Code, models.Body.String())
		}
		if got := f.module.writer.Snapshot().PendingRecords; got != 2 {
			t.Fatalf("model completion entered billed cache: %d", got)
		}
	}
	exec(`DROP TRIGGER fail_accounting`)
	// A new generation retries retained facts first, then reads the updated quota.
	resumed := f.request(t, http.MethodPost, "/v1/chat/completions", `{"model":"`+serverRouteOpenAIModel+`","messages":[{"role":"user","content":"test"}]}`, false)
	if resumed.Code != 200 {
		t.Fatalf("retry did not resume: %d %s", resumed.Code, resumed.Body.String())
	}
	if f.module.writer.Snapshot().PendingRecords != 0 {
		t.Fatal("resumed with unresolved records")
	}
	persisted, err := f.module.store.GetProxyRequest(context.Background(), snapshot.RequestID())
	if err != nil || persisted.BilledNanoUSD == nil || *persisted.BilledNanoUSD <= 0 || persisted.Outcome != domain.RequestOutcomeSucceeded {
		t.Fatalf("recovered facts = %#v, %v", persisted, err)
	}
	// Closing with another failed event must keep the database open for retry.
	second := accountingRequest(t, f)
	exec(`CREATE TRIGGER fail_accounting BEFORE INSERT ON usage_events BEGIN SELECT RAISE(ABORT, 'sensitive-db-failure'); END`)
	record := accountingRecord()
	record.EventID = "close-failure"
	other := usage.NewManager(1)
	other.Publish(second.Context(), record)
	other.Stop()
	if errClose := f.module.Close(context.Background()); !errors.Is(errClose, domain.ErrAccountingUnavailable) {
		t.Fatalf("Close = %v", errClose)
	}
	if _, errVersion := f.module.store.SchemaVersion(context.Background()); errVersion != nil {
		t.Fatal("Close closed DB despite unresolved records")
	}
	exec(`DROP TRIGGER fail_accounting`)
	if errClose := f.module.Close(context.Background()); errClose != nil {
		t.Fatal(errClose)
	}
}

func TestModuleRestartAllowsGenerationWithInterruptedBillingMarker(t *testing.T) {
	f := newServerRouteFixture(t, false)
	request := accountingRequest(t, f)
	snapshot, _ := carpoolruntime.AuthorizationFromContext(request.Context())
	manager := usage.NewManager(1)
	manager.Publish(request.Context(), accountingRecord())
	manager.Stop()
	before, err := f.module.store.GetProxyRequest(context.Background(), snapshot.RequestID())
	if err != nil || before.BilledNanoUSD == nil {
		t.Fatalf("before restart = %#v, %v", before, err)
	}
	if err := f.module.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	catalog := coreauth.NewManager(nil, nil, nil)
	if _, err := catalog.Register(context.Background(), &coreauth.Auth{ID: "route-a-openai-1", Provider: "openai", Status: coreauth.StatusActive}); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(context.Background(), moduleTestConfig(f.databasePath, true), f.configPath, catalog)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := reopened.Close(context.Background()); errClose != nil {
			t.Error(errClose)
		}
	}()
	if errGate := reopened.writer.CheckAdmission(context.Background()); errGate != nil {
		t.Fatalf("restart gate = %v", errGate)
	}
	recovered, err := reopened.store.GetProxyRequest(context.Background(), snapshot.RequestID())
	if err != nil || recovered.Outcome != domain.RequestOutcomeIncomplete {
		t.Fatalf("recovery = %#v, %v", recovered, err)
	}
	if recovered.ReasonCode != "process_interrupted" || recovered.BilledNanoUSD == nil || *recovered.BilledNanoUSD != *before.BilledNanoUSD {
		t.Fatalf("restart lost marker or changed confirmed subtotal: %#v", recovered)
	}
	if _, err := reopened.control.AuthorizeProxy(context.Background(), f.passenger.ID, f.apiKeyID, "test", http.MethodPost, "/v1/chat/completions", false); err != nil {
		t.Fatalf("restart authorization = %v", err)
	}
	if reopened.writer.Snapshot().ReconciliationRequired {
		t.Fatal("historical interruption blocked generation")
	}
}

func TestModuleRestartIgnoresInterruptedModelReadAndLegacyHistory(t *testing.T) {
	f := newServerRouteFixture(t, false)
	snapshot, err := f.module.control.AuthorizeProxy(context.Background(), f.passenger.ID, f.apiKeyID, "test", http.MethodGet, "/v1/models", false)
	if err != nil {
		t.Fatal(err)
	}
	modelRead, err := f.module.store.GetProxyRequest(context.Background(), snapshot.RequestID())
	if err != nil || modelRead.BillingStatus != "not_billable" {
		t.Fatalf("model read = %#v, %v", modelRead, err)
	}
	legacy := accountingRequest(t, f)
	legacySnapshot, _ := carpoolruntime.AuthorizationFromContext(legacy.Context())
	raw, err := sql.Open("sqlite", "file:"+filepath.ToSlash(f.databasePath))
	if err != nil {
		t.Fatal(err)
	}
	if _, errUpdate := raw.Exec(`UPDATE proxy_requests SET billing_period_id = NULL WHERE request_id = ?`, legacySnapshot.RequestID()); errUpdate != nil {
		t.Fatal(errUpdate)
	}
	if errClose := raw.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	if errClose := f.module.Close(context.Background()); errClose != nil {
		t.Fatal(errClose)
	}
	reopened, err := Open(context.Background(), moduleTestConfig(f.databasePath, true), f.configPath, coreauth.NewManager(nil, nil, nil))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if errClose := reopened.Close(context.Background()); errClose != nil {
			t.Error(errClose)
		}
	}()
	if errGate := reopened.writer.CheckAdmission(context.Background()); errGate != nil {
		t.Fatalf("non-billable history blocked accounting: %v", errGate)
	}
}

type blockedAccountingStore struct {
	*carpoolsqlite.Store
	entered chan struct{}
	release chan struct{}
}

func (s *blockedAccountingStore) RecordUsageEventBilled(ctx context.Context, event domain.UsageEvent, period string, cost *int64, status, reason string) (domain.UsageEvent, error) {
	close(s.entered)
	<-s.release
	return s.Store.RecordUsageEventBilled(ctx, event, period, cost, status, reason)
}

func TestModuleCloseCanceledBudgetKeepsInFlightObserverDatabaseOpen(t *testing.T) {
	f := newServerRouteFixture(t, false)
	request := accountingRequest(t, f)
	if errClose := f.module.writer.Close(context.Background()); errClose != nil {
		t.Fatal(errClose)
	}
	blocked := &blockedAccountingStore{Store: f.module.store, entered: make(chan struct{}), release: make(chan struct{})}
	writer, err := accounting.NewWriter(blocked, accounting.Config{})
	if err != nil {
		t.Fatal(err)
	}
	f.module.writer = writer
	manager := usage.NewManager(1)
	published := make(chan struct{})
	go func() { manager.Publish(request.Context(), accountingRecord()); close(published) }()
	<-blocked.entered
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if errClose := f.module.Close(canceled); !errors.Is(errClose, context.Canceled) {
		t.Fatalf("Close budget = %v", errClose)
	}
	if _, errVersion := f.module.store.SchemaVersion(context.Background()); errVersion != nil {
		t.Fatal("timed-out Close closed observer database")
	}
	close(blocked.release)
	<-published
	manager.Stop()
	if errClose := f.module.Close(context.Background()); errClose != nil {
		t.Fatal(errClose)
	}
}

func TestModuleGlobalUsageAndCompletionCannotPoisonAccountingGate(t *testing.T) {
	f := newServerRouteFixture(t, false)
	manager := usage.NewManager(1)
	manager.Register(scopedAccountingPlugin{writer: f.module.writer})
	record := accountingRecord()
	record.RequestID = "global-request-not-in-carpool"
	manager.Publish(context.Background(), record)
	manager.Stop()
	f.module.observeRequestCompletion(context.Background(), pluginapi.RequestCompletion{
		RequestID: record.RequestID, Outcome: pluginapi.RequestCompletionSucceeded,
	})
	if metrics := f.module.writer.Snapshot(); metrics.PendingRecords != 0 || metrics.WriteFailures != 0 {
		t.Fatalf("global request poisoned billing: %#v", metrics)
	}
	if errGate := f.module.writer.CheckAdmission(context.Background()); errGate != nil {
		t.Fatalf("global request blocked generation: %v", errGate)
	}
	// The same adapter must still persist scoped deliveries lacking an observer.
	request := accountingRequest(t, f)
	snapshot, _ := carpoolruntime.AuthorizationFromContext(request.Context())
	ctx := carpoolruntime.WithAuthorization(context.Background(), snapshot)
	scopedManager := usage.NewManager(1)
	scopedManager.Register(scopedAccountingPlugin{writer: f.module.writer})
	scopedManager.Publish(ctx, record)
	scopedManager.Stop()
	f.module.observeRequestCompletion(ctx, pluginapi.RequestCompletion{
		RequestID: "ignored-record-id", Outcome: pluginapi.RequestCompletionSucceeded,
	})
	persisted, err := f.module.store.GetProxyRequest(context.Background(), snapshot.RequestID())
	if err != nil || persisted.BilledNanoUSD == nil || persisted.Outcome != domain.RequestOutcomeSucceeded {
		t.Fatalf("scoped delivery lost: %#v, %v", persisted, err)
	}
}
