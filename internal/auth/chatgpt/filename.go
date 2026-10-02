package chatgpt

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
)

// ProviderName is the provider identifier for credentials created by this flow.
const ProviderName = "chatgpt"

// SubjectHash derives a stable, non-reversible identifier for the signed-in account
// from the access token's `sub` claim. It is used for auth file naming only, so that
// signing in twice for the same account updates one file instead of creating two.
//
// Nothing else in this package reads token claims: request shaping stays byte-aligned
// with pi, which never inspects the access token.
func SubjectHash(accessToken string) string {
	subject := jwtClaim(accessToken, "sub")
	if subject == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(subject))
	return hex.EncodeToString(sum[:])[:12]
}

// EmailFromIDToken extracts the `email` claim for informational display. The value is
// never used for authentication or account routing.
func EmailFromIDToken(idToken string) string {
	return jwtClaim(idToken, "email")
}

// CredentialFileName returns the auth file name for a credential. Files are prefixed
// with the provider so that they are recognisable next to codex-*.json files.
func CredentialFileName(subjectHash string) string {
	hash := strings.TrimSpace(subjectHash)
	if hash == "" {
		hash = "unknown"
	}
	return fmt.Sprintf("%s-%s.json", ProviderName, hash)
}

// jwtClaim reads a single claim from an unverified JWT payload. Signature checking is
// intentionally skipped: these values are display metadata, never authorization input.
func jwtClaim(token, claim string) string {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" || claim == "" {
		return ""
	}
	parts := strings.Split(trimmed, ".")
	if len(parts) < 2 {
		return ""
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimRight(parts[1], "="))
	if err != nil {
		return ""
	}
	var claims map[string]any
	if err := json.Unmarshal(payload, &claims); err != nil {
		return ""
	}
	value, ok := claims[claim].(string)
	if !ok {
		return ""
	}
	return strings.TrimSpace(value)
}
