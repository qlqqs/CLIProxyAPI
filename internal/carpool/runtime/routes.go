package runtime

import (
	"net/http"
	"strings"
)

const (
	RouteRejectionUnsupported = "route_not_supported"
	RouteRejectionUpgrade     = "upgrade_not_supported"
)

// ProxyRoutePolicy describes whether a carpool API key may use a proxy route.
type ProxyRoutePolicy struct {
	Allowed      bool
	SourceFormat string
	ReasonCode   string
}

// CarpoolProxyRoutePolicy implements the explicit first-phase proxy allowlist.
func CarpoolProxyRoutePolicy(method, path string, upgrade bool) ProxyRoutePolicy {
	method = strings.ToUpper(strings.TrimSpace(method))
	path = strings.TrimSpace(path)
	if upgrade {
		return ProxyRoutePolicy{ReasonCode: RouteRejectionUpgrade}
	}

	switch {
	case method == http.MethodGet && path == "/v1/models":
		return ProxyRoutePolicy{Allowed: true, SourceFormat: "openai"}
	case method == http.MethodPost && (path == "/v1/chat/completions" || path == "/v1/completions" || path == "/v1/responses" || path == "/responses"):
		return ProxyRoutePolicy{Allowed: true, SourceFormat: "openai"}
	case method == http.MethodPost && path == "/v1/messages":
		return ProxyRoutePolicy{Allowed: true, SourceFormat: "claude"}
	case method == http.MethodPost && path == "/backend-api/codex/responses":
		return ProxyRoutePolicy{Allowed: true, SourceFormat: "openai-response"}
	case method == http.MethodGet && path == "/v1beta/models":
		return ProxyRoutePolicy{Allowed: true, SourceFormat: "gemini"}
	case method == http.MethodGet && allowedGeminiModelRead(path):
		return ProxyRoutePolicy{Allowed: true, SourceFormat: "gemini"}
	case method == http.MethodPost && path == "/v1beta/interactions":
		return ProxyRoutePolicy{Allowed: true, SourceFormat: "gemini"}
	case method == http.MethodPost && allowedGeminiModelAction(path):
		return ProxyRoutePolicy{Allowed: true, SourceFormat: "gemini"}
	default:
		return ProxyRoutePolicy{ReasonCode: RouteRejectionUnsupported}
	}
}

func allowedGeminiModelAction(path string) bool {
	if !strings.HasPrefix(path, "/v1beta/models/") {
		return false
	}
	for _, suffix := range []string{":generateContent", ":streamGenerateContent"} {
		if strings.HasSuffix(path, suffix) && len(path) > len("/v1beta/models/")+len(suffix) {
			return true
		}
	}
	return false
}

func allowedGeminiModelRead(path string) bool {
	const prefix = "/v1beta/models/"
	if !strings.HasPrefix(path, prefix) {
		return false
	}
	modelID := strings.TrimPrefix(path, prefix)
	return modelID != "" && !strings.ContainsAny(modelID, "/:")
}
