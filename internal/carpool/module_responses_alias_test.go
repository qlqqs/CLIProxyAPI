package carpool

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestRootResponsesCarpoolQuotaAndConcurrency(t *testing.T) {
	f := newServerRouteFixture(t, false)
	member, errMember := f.module.store.CurrentMembership(t.Context(), f.passenger.ID)
	if errMember != nil {
		t.Fatal(errMember)
	}
	one := 1
	if errSet := f.module.store.SetMemberLimits(t.Context(), member.ID, domain.MemberLimitsUpdate{UserConcurrencySet: true, UserConcurrency: &one}, nil); errSet != nil {
		t.Fatal(errSet)
	}
	// Each request must release the same single user slot, including handler failures.
	for _, body := range []string{`{"model":`, `{"model":"` + serverRouteOpenAIModel + `","input":"test"}`, `{"model":"` + serverRouteOpenAIModel + `","input":"test","stream":true}`} {
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		request := httptest.NewRequest(http.MethodPost, "/responses", strings.NewReader(body)).WithContext(ctx)
		request.Header.Set("Authorization", "Bearer "+f.accessToken)
		response := httptest.NewRecorder()
		f.engine.ServeHTTP(response, request)
		cancel()
		want := http.StatusOK
		if body == `{"model":` {
			want = http.StatusBadRequest
		}
		if response.Code != want {
			t.Fatalf("status=%d want=%d body=%s", response.Code, want, response.Body.String())
		}
		if strings.Contains(body, `"stream":true`) && (!strings.Contains(response.Header().Get("Content-Type"), "text/event-stream") || !strings.Contains(response.Body.String(), "response.completed")) {
			t.Fatalf("missing Responses SSE completion: %s", response.Body.String())
		}
	}
	zero := int64(0)
	if errSet := f.module.store.SetMemberLimits(t.Context(), member.ID, domain.MemberLimitsUpdate{MonthlySet: true, MonthlyNanoUSD: &zero}, nil); errSet != nil {
		t.Fatal(errSet)
	}
	before, _ := f.recorder.snapshot()
	for _, path := range []string{"/responses", "/v1/responses"} {
		response := f.request(t, http.MethodPost, path, `{"model":"`+serverRouteOpenAIModel+`","input":"test"}`, false)
		if response.Code != http.StatusForbidden {
			t.Fatalf("quota bypassed on %s: %d %s", path, response.Code, response.Body.String())
		}
	}
	after, _ := f.recorder.snapshot()
	if len(after) != len(before) {
		t.Fatal("quota rejection reached upstream")
	}
}

func TestRootResponsesCarpoolMissingAndInvalidKeys(t *testing.T) {
	f := newServerRouteFixture(t, false)
	for _, route := range []struct{ method, path string }{
		{http.MethodPost, "/responses"},
		{http.MethodGet, "/responses"},
		{http.MethodPost, "/responses/compact"},
	} {
		for _, key := range []string{"", "invalid-key", "cpk_v1_invalid.invalid"} {
			request := httptest.NewRequest(route.method, route.path, strings.NewReader(`{}`))
			if key != "" {
				request.Header.Set("Authorization", "Bearer "+key)
			}
			response := httptest.NewRecorder()
			f.engine.ServeHTTP(response, request)
			if response.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s: status=%d body=%s", route.method, route.path, response.Code, response.Body.String())
			}
		}
	}
	selected, candidates := f.recorder.snapshot()
	if len(selected) != 0 || len(candidates) != 0 {
		t.Fatal("invalid credentials reached upstream selection")
	}
}
