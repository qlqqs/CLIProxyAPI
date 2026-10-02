package chatgpt

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
)

// PKCECodes holds the verifier/challenge pair for an authorization request.
type PKCECodes struct {
	CodeVerifier  string
	CodeChallenge string
}

// GeneratePKCECodes creates a fresh PKCE verifier and its S256 challenge (RFC 7636).
func GeneratePKCECodes() (*PKCECodes, error) {
	verifier, err := generateCodeVerifier()
	if err != nil {
		return nil, fmt.Errorf("failed to generate code verifier: %w", err)
	}
	return &PKCECodes{
		CodeVerifier:  verifier,
		CodeChallenge: generateCodeChallenge(verifier),
	}, nil
}

func generateCodeVerifier() (string, error) {
	// 96 random bytes encode to 128 URL-safe characters, within the 43-128 range
	// required for a PKCE verifier.
	raw := make([]byte, 96)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("failed to generate random bytes: %w", err)
	}
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(raw), nil
}

func generateCodeChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.URLEncoding.WithPadding(base64.NoPadding).EncodeToString(sum[:])
}
