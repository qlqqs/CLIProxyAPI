package httpapi

import (
	"fmt"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestAdministratorUsageRequestsHTTPContract(t *testing.T) {
	fixture := newCarpoolHTTPFlowFixture(t)
	cookie, _ := loginForAdminCompletion(t, fixture, "operator", "administrator-pass")
	user, errUser := fixture.store.CreateUser(t.Context(), domain.User{Username: "usage-passenger", DefaultDisplayName: "Passenger", Role: domain.UserRolePassenger, Status: domain.UserStatusActive, PasswordHash: "test-hash"})
	if errUser != nil {
		t.Fatal(errUser)
	}
	key, errKey := fixture.store.CreateAPIKey(t.Context(), domain.APIKey{UserID: user.ID, Name: "test", SecretDigest: []byte("test-digest")})
	if errKey != nil {
		t.Fatal(errKey)
	}
	for i := 0; i < 26; i++ {
		_, errBegin := fixture.store.BeginProxyRequest(t.Context(), domain.ProxyRequest{RequestID: fmt.Sprintf("request-%02d", i), UserID: user.ID, APIKeyID: key.KeyID, RequestedModel: "wanted", SourceFormat: "openai", StartedAt: fixture.now().Add(-time.Minute), Outcome: domain.RequestOutcomeRejected}, nil)
		if errBegin != nil {
			t.Fatal(errBegin)
		}
	}
	const path = "/carpool/api/v1/admin/usage/requests"
	first := fixture.request(t, http.MethodGet, path, nil, cookie, "", false)
	assertHTTPStatus(t, first, http.StatusOK)
	body := decodeResponseObject(t, first)
	items, ok := body["items"].([]any)
	if !ok || len(items) != 25 || body["total"] != float64(26) {
		t.Fatalf("first page = %#v", body)
	}
	period, ok := body["period"].(map[string]any)
	if !ok || period["from"] != "2026-09-04T00:00:00Z" || period["to"] != fixture.now().Format(time.RFC3339) {
		t.Fatalf("resolved period = %#v", body["period"])
	}
	cursor := stringField(t, body, "next_cursor")
	second := fixture.request(t, http.MethodGet, path+"?cursor="+url.QueryEscape(cursor), nil, cookie, "", false)
	assertHTTPStatus(t, second, http.StatusOK)
	secondBody := decodeResponseObject(t, second)
	secondItems, ok := secondBody["items"].([]any)
	if !ok || len(secondItems) != 1 || secondBody["total"] != float64(26) || secondBody["next_cursor"] != nil {
		t.Fatalf("second page = %#v", secondBody)
	}
	for _, query := range []string{"limit=0", "limit=1", "limit=24", "limit=26", "limit=no", "cursor=!", "cursor=" + url.QueryEscape(cursor) + "&model=wanted", "cursor=" + url.QueryEscape(cursor) + "&period=7d"} {
		response := fixture.request(t, http.MethodGet, path+"?"+query, nil, cookie, "", false)
		assertHTTPStatus(t, response, http.StatusUnprocessableEntity)
	}
	valid := fixture.request(t, http.MethodGet, path+"?limit=25", nil, cookie, "", false)
	assertHTTPStatus(t, valid, http.StatusOK)
	exact := fixture.request(t, http.MethodGet, path+"?request_id=request-00", nil, cookie, "", false)
	assertHTTPStatus(t, exact, http.StatusOK)
	if exactBody := decodeResponseObject(t, exact); exactBody["total"] != float64(1) {
		t.Fatalf("exact ID = %#v", exactBody)
	}
	prefix := fixture.request(t, http.MethodGet, path+"?request_id=request-0", nil, cookie, "", false)
	assertHTTPStatus(t, prefix, http.StatusOK)
	if prefixBody := decodeResponseObject(t, prefix); prefixBody["total"] != float64(0) {
		t.Fatalf("prefix unexpectedly matched = %#v", prefixBody)
	}
	detail := fixture.request(t, http.MethodGet, path+"/request-00", nil, cookie, "", false)
	assertHTTPStatus(t, detail, http.StatusOK)
	detailBody := decodeResponseObject(t, detail)
	events, ok := detailBody["events"].([]any)
	if !ok || len(events) != 0 || detailBody["billed_usd"] != nil {
		t.Fatalf("unknown/empty detail = %#v", detailBody)
	}
}

func TestUsageEventResponsePreservesSeparateCacheBuckets(t *testing.T) {
	cached, read, write := int64(19), int64(12), int64(7)
	response := usageEventResponse(domain.UsageEvent{UsageKnown: true, CachedTokens: &cached, CacheReadTokens: &read, CacheWriteTokens: &write})
	for field, want := range map[string]int64{"cached_tokens": 19, "cache_read_tokens": 12, "cache_write_tokens": 7} {
		if got := response[field]; got != want {
			t.Fatalf("%s = %#v, want %d", field, got, want)
		}
	}
	zero := int64(0)
	response = usageEventResponse(domain.UsageEvent{CacheReadTokens: &zero})
	if response["cache_read_tokens"] != int64(0) || response["cache_write_tokens"] != nil || response["cached_tokens"] != nil {
		t.Fatalf("missing cache buckets fabricated zero: %#v", response)
	}
}
