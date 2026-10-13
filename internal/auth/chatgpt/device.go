package chatgpt

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// LoadOrCreateDeviceID returns the installation UUID used as the OAuth "agent host".
//
// The authorization server expects a stable per-installation identifier in
// ext_agent_host_id. pi keeps this value in its own settings store; here it lives in a
// small file next to the auth directory.
func LoadOrCreateDeviceID(path string) (string, error) {
	trimmedPath := strings.TrimSpace(path)
	if trimmedPath == "" {
		return "", fmt.Errorf("chatgpt: device id path is required")
	}

	if raw, err := os.ReadFile(trimmedPath); err == nil {
		if candidate := strings.TrimSpace(string(raw)); uuidPattern.MatchString(candidate) {
			return strings.ToLower(candidate), nil
		}
	} else if !os.IsNotExist(err) {
		return "", fmt.Errorf("chatgpt: read device id: %w", err)
	}

	generated, err := newUUID()
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(trimmedPath), 0o700); err != nil {
		return "", fmt.Errorf("chatgpt: create device id directory: %w", err)
	}
	if err := os.WriteFile(trimmedPath, []byte(generated), 0o600); err != nil {
		return "", fmt.Errorf("chatgpt: write device id: %w", err)
	}
	return generated, nil
}

// newUUID returns a random RFC 4122 version 4 UUID.
func newUUID() (string, error) {
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return "", fmt.Errorf("chatgpt: generate uuid: %w", err)
	}
	raw[6] = (raw[6] & 0x0f) | 0x40
	raw[8] = (raw[8] & 0x3f) | 0x80
	encoded := hex.EncodeToString(raw)
	return fmt.Sprintf("%s-%s-%s-%s-%s", encoded[0:8], encoded[8:12], encoded[12:16], encoded[16:20], encoded[20:32]), nil
}
