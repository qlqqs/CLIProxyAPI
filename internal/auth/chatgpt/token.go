package chatgpt

import (
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/tokenfile"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/misc"
)

// ChatGPTTokenStorage is the on-disk credential written by this flow.
//
// The field set mirrors what pi persists for the "Sign in with ChatGPT" provider,
// mapped onto this repo's auth-file conventions:
//
//	pi OAuthCredential   this file
//	access               access_token
//	refresh              refresh_token
//	expires (epoch ms)   expired (RFC3339)
//	clientId             client_id
//	scopes               scopes
//
// pi does not persist the ID token because it is never used to identify the user;
// the same is true here, so this struct has no id_token field.
type ChatGPTTokenStorage struct {
	// AccessToken authenticates upstream calls to the Responses API.
	AccessToken string `json:"access_token"`
	// RefreshToken obtains new access tokens; it is replayed with ClientID.
	RefreshToken string `json:"refresh_token"`
	// ClientID is the client id the authorization server issued dynamically for
	// this login. Refresh requests are rejected without it.
	ClientID string `json:"client_id"`
	// Scopes are granted scopes; DirectTokenScope must be present.
	Scopes []string `json:"scopes,omitempty"`
	// Email is informational only, when derivable from the ID token.
	Email string `json:"email,omitempty"`
	// LastRefresh is when the credential was last obtained or refreshed.
	LastRefresh string `json:"last_refresh"`
	// Expire is the access token expiry (RFC3339), already reduced by ExpiryMargin.
	Expire string `json:"expired"`
	// Type marks the provider that owns this file.
	Type string `json:"type"`

	// Metadata carries store-injected values (for example "disabled"); it is
	// flattened into the top-level JSON object on save.
	Metadata map[string]any `json:"-"`
}

// SetMetadata lets the auth store inject metadata before persisting.
func (ts *ChatGPTTokenStorage) SetMetadata(meta map[string]any) {
	ts.Metadata = meta
}

// SaveTokenToFile writes the credential to authFilePath.
func (ts *ChatGPTTokenStorage) SaveTokenToFile(authFilePath string) error {
	misc.LogSavingCredentials(authFilePath)
	ts.Type = ProviderName

	data, errMerge := misc.MergeMetadata(ts, ts.Metadata)
	if errMerge != nil {
		return fmt.Errorf("failed to merge metadata: %w", errMerge)
	}
	return tokenfile.WriteJSON(authFilePath, data)
}
