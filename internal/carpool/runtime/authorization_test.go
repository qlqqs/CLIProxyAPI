package runtime

import (
	"context"
	"net/http"
	"reflect"
	"testing"
)

func TestAuthorizationSnapshotCopiesAndSortsAccounts(t *testing.T) {
	accounts := map[string]AccountSnapshot{
		"auth-b": {SafeLabel: "B"},
		"auth-a": {SafeLabel: "A"},
	}
	snapshot := NewAuthorizationSnapshot("request", "user", "key", "car", "member", "caller", accounts)
	delete(accounts, "auth-a")
	accounts["auth-c"] = AccountSnapshot{SafeLabel: "C"}
	if got, want := snapshot.AuthIDs(), []string{"auth-a", "auth-b"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("AuthIDs() = %#v, want %#v", got, want)
	}
	account, ok := snapshot.Account("auth-a")
	if !ok || account.AuthID != "auth-a" || account.SafeLabel != "A" {
		t.Fatalf("Account(auth-a) = (%#v, %t)", account, ok)
	}
}

func TestAuthorizationSnapshotContextRoundTrip(t *testing.T) {
	snapshot := NewAuthorizationSnapshot("request", "user", "key", "car", "member", "caller", nil)
	ctx := WithAuthorization(context.Background(), snapshot)
	got, ok := AuthorizationFromContext(ctx)
	if !ok || got != snapshot {
		t.Fatalf("AuthorizationFromContext() = (%p, %t), want %p", got, ok, snapshot)
	}
}

func TestCarpoolProxyRoutePolicy(t *testing.T) {
	tests := []struct {
		name    string
		method  string
		path    string
		upgrade bool
		allowed bool
		reason  string
	}{
		{name: "chat", method: http.MethodPost, path: "/v1/chat/completions", allowed: true},
		{name: "responses HTTP", method: http.MethodPost, path: "/v1/responses", allowed: true},
		{name: "responses websocket", method: http.MethodGet, path: "/v1/responses", upgrade: true, reason: RouteRejectionUpgrade},
		{name: "claude", method: http.MethodPost, path: "/v1/messages", allowed: true},
		{name: "claude count rejected", method: http.MethodPost, path: "/v1/messages/count_tokens", reason: RouteRejectionUnsupported},
		{name: "codex direct", method: http.MethodPost, path: "/backend-api/codex/responses", allowed: true},
		{name: "codex compact rejected", method: http.MethodPost, path: "/backend-api/codex/responses/compact", reason: RouteRejectionUnsupported},
		{name: "gemini model list", method: http.MethodGet, path: "/v1beta/models", allowed: true},
		{name: "gemini model read", method: http.MethodGet, path: "/v1beta/models/gemini-2.5-pro", allowed: true},
		{name: "gemini nested model read", method: http.MethodGet, path: "/v1beta/models/group/gemini-2.5-pro", reason: RouteRejectionUnsupported},
		{name: "gemini model action read", method: http.MethodGet, path: "/v1beta/models/gemini-2.5-pro:generateContent", reason: RouteRejectionUnsupported},
		{name: "gemini generate", method: http.MethodPost, path: "/v1beta/models/gemini-2.5-pro:generateContent", allowed: true},
		{name: "gemini stream", method: http.MethodPost, path: "/v1beta/models/gemini-2.5-pro:streamGenerateContent", allowed: true},
		{name: "gemini count rejected", method: http.MethodPost, path: "/v1beta/models/gemini-2.5-pro:countTokens", reason: RouteRejectionUnsupported},
		{name: "realtime", method: http.MethodPost, path: "/v1/realtime", reason: RouteRejectionUnsupported},
		{name: "images", method: http.MethodPost, path: "/v1/images/generations", reason: RouteRejectionUnsupported},
		{name: "unknown gemini action", method: http.MethodPost, path: "/v1beta/models/gemini-2.5-pro:delete", reason: RouteRejectionUnsupported},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := CarpoolProxyRoutePolicy(test.method, test.path, test.upgrade)
			if got.Allowed != test.allowed || got.ReasonCode != test.reason {
				t.Fatalf("CarpoolProxyRoutePolicy() = %#v", got)
			}
		})
	}
}
