package carpool

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	carpoolruntime "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/runtime"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

type checkerBrokenBody struct{ cancel context.CancelFunc }

func (b checkerBrokenBody) Read([]byte) (int, error) {
	if b.cancel != nil {
		b.cancel()
	}
	return 0, errors.New("synthetic body read error")
}
func (checkerBrokenBody) Close() error { return nil }

func TestCheckerModuleBodyReadFailureCompletesAndReleases(t *testing.T) {
	f := newServerRouteFixture(t, false)
	member, errMember := f.module.store.CurrentMembership(t.Context(), f.passenger.ID)
	if errMember != nil {
		t.Fatal(errMember)
	}
	one := 1
	if errSet := f.module.store.SetMemberLimits(t.Context(), member.ID, domain.MemberLimitsUpdate{UserConcurrencySet: true, UserConcurrency: &one}, nil); errSet != nil {
		t.Fatal(errSet)
	}
	seen := make(map[string]bool)
	for _, canceled := range []bool{false, true, false} {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
		request.Body = checkerBrokenBody{}
		if canceled {
			request.Body = checkerBrokenBody{cancel: cancel}
		}
		request.Header.Set("Authorization", "Bearer "+f.accessToken)
		response := httptest.NewRecorder()
		f.engine.ServeHTTP(response, request)
		cancel()
		if response.Code != http.StatusBadRequest {
			t.Fatalf("body read response=%d %s", response.Code, response.Body.String())
		}
		facts, errFacts := f.module.store.ListUsageRequests(t.Context(), domain.UsageRequestFilter{From: time.Now().Add(-time.Hour), To: time.Now().Add(time.Hour)}, time.Time{}, "", 100)
		if errFacts != nil || len(facts) == 0 {
			t.Fatalf("body failure did not persist actual authorization: %v %v", facts, errFacts)
		}
		var fact domain.ProxyRequest
		for _, item := range facts {
			if !seen[item.Request.RequestID] {
				if fact.RequestID != "" {
					t.Fatal("one HTTP request created multiple facts")
				}
				fact = item.Request
				seen[fact.RequestID] = true
			}
		}
		want := domain.RequestOutcomeRejected
		if canceled {
			want = domain.RequestOutcomeCanceled
		}
		if fact.Outcome != want || fact.CompletedAt == nil || fact.UpstreamAttempted {
			t.Fatalf("body failure fact=%+v", fact)
		}
	}
	selected, _ := f.recorder.snapshot()
	if len(selected) != 0 {
		t.Fatalf("body read failures called upstream: %v", selected)
	}
	// Another request must acquire the same single user slot and execute normally.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	snapshot, errAuthorize := f.module.Control().AuthorizeProxy(ctx, f.passenger.ID, f.apiKeyID, "checker", http.MethodPost, "/v1/chat/completions", false)
	if errAuthorize != nil {
		t.Fatalf("body failure leaked its permit: %v", errAuthorize)
	}
	snapshot.Release()
}

func TestCheckerCompletionOncePreservesGenuineResultAcrossSnapshotCopies(t *testing.T) {
	f := newServerRouteFixture(t, false)
	request := accountingRequest(t, f)
	snapshot, _ := carpoolruntime.AuthorizationFromContext(request.Context())
	defer snapshot.Release()
	f.module.observeRequestCompletion(request.Context(), pluginapi.RequestCompletion{Outcome: pluginapi.RequestCompletionRejected, StatusCode: http.StatusUnprocessableEntity})
	copied := snapshot.WithRelease(nil)
	copiedCtx := carpoolruntime.WithAuthorization(context.Background(), copied)
	f.module.observeHTTPFallbackCompletion(copiedCtx, pluginapi.RequestCompletion{Outcome: pluginapi.RequestCompletionCanceled})
	fact, errFact := f.module.store.GetProxyRequest(t.Context(), snapshot.RequestID())
	if errFact != nil || fact.Outcome != domain.RequestOutcomeRejected || fact.StatusClass != "4xx" {
		t.Fatalf("genuine fact changed: %+v %v", fact, errFact)
	}
	if errAdmission := f.module.writer.CheckAdmission(t.Context()); errAdmission != nil {
		t.Fatalf("conflicting HTTP fallback poisoned accounting: %v", errAdmission)
	}
}

func TestCheckerFallbackAfterPossibleExecutionRemainsIncomplete(t *testing.T) {
	f := newServerRouteFixture(t, false)
	request := accountingRequest(t, f)
	snapshot, _ := carpoolruntime.AuthorizationFromContext(request.Context())
	defer snapshot.Release()
	if errValidate := coreexecutor.ValidateRequest(request.Context(), "openai", coreexecutor.Request{Model: serverRouteOpenAIModel}); errValidate != nil {
		t.Fatal(errValidate)
	}
	f.module.observeHTTPFallbackCompletion(request.Context(), pluginapi.RequestCompletion{Outcome: pluginapi.RequestCompletionCanceled})
	fact, errFact := f.module.store.GetProxyRequest(t.Context(), snapshot.RequestID())
	if errFact != nil || fact.Outcome != domain.RequestOutcomeIncomplete || !fact.UpstreamAttempted {
		t.Fatalf("unjoined execution was fabricated as finished: %+v %v", fact, errFact)
	}
}
