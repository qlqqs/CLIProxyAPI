// Package chatgpt implements OpenAI's "Sign in with ChatGPT" OAuth flow, mirroring
// the flow used by the pi coding agent (packages/ai/src/auth/oauth/openai-chatgpt.ts).
//
// This flow is deliberately distinct from the package in ../codex:
//
//   - Client registration: "dynamic_agent_client" — the authorization server issues a
//     fresh client_id per login and returns it on the callback URL. The issued value
//     (oaiapp_...) must be stored and replayed on every refresh.
//   - Endpoints live under /api/accounts/* instead of /oauth/*.
//   - The grant requests the direct-token scope and pins the API resource, which is what
//     makes the resulting access token usable against https://api.openai.com/v1 directly
//     (the codex backend at https://chatgpt.com/backend-api/codex rejects it).
//   - No id_token-derived account id is available: the authorization server returns no
//     account_id claim for this client, so credentials carry no Chatgpt-Account-Id.
//
// Request bodies and the authorization URL are built in the same field order as pi so
// that upstream sees byte-identical requests.
package chatgpt

import "time"

// OAuth endpoints and constants for the "Sign in with ChatGPT" flow.
const (
	// AuthorizeURL is the authorization endpoint (note: /api/accounts/authorize,
	// not /oauth/authorize).
	AuthorizeURL = "https://auth.openai.com/api/accounts/authorize"
	// TokenURL is the token endpoint (note: /api/accounts/oauth/token, not /oauth/token).
	TokenURL = "https://auth.openai.com/api/accounts/oauth/token"
	// DynamicClientID is the sentinel client id that asks the authorization server to
	// register a client dynamically; the issued id arrives on the callback URL.
	DynamicClientID = "dynamic_agent_client"
	// Resource is the API audience the issued token is scoped to.
	Resource = "https://api.openai.com/v1"
	// DefaultBaseURL is the upstream base URL for credentials issued by this flow.
	DefaultBaseURL = "https://api.openai.com/v1"
	// RedirectURI is the loopback callback pi registers. The flow in this package is
	// manual-paste only, so no listener is started on this address; the URI is still
	// sent verbatim because it is bound into the authorization grant.
	RedirectURI = "http://127.0.0.1:1455/auth/callback"
	// DirectTokenScope is the scope that permits direct API access with the token.
	DirectTokenScope = "chatgpt.tokens.use.direct"
	// Scope is the full scope string requested at authorization time.
	Scope = "openid profile email offline_access resource.invoke " + DirectTokenScope
	// DefaultAgentNameHint identifies this integration to the authorization server.
	DefaultAgentNameHint = "Pi"
)

// ExpiryMargin is subtracted from the token lifetime so credentials refresh early.
// pi uses a 3 minute margin.
const ExpiryMargin = 3 * time.Minute
