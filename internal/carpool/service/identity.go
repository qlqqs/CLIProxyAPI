package service

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/crypto/argon2"
	"golang.org/x/text/unicode/norm"
)

const (
	UserAPIKeyPrefix = "cpk_v1_"
	sessionPrefix    = "cps_v1_"
)

var usernamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9._-]{1,62}[a-z0-9])?$`)

// PasswordParams controls Argon2id password hashing.
type PasswordParams struct {
	Memory      uint32
	Iterations  uint32
	Parallelism uint8
	SaltLength  uint32
	KeyLength   uint32
}

// DefaultPasswordParams returns the production password hashing parameters.
func DefaultPasswordParams() PasswordParams {
	return PasswordParams{
		Memory:      64 * 1024,
		Iterations:  3,
		Parallelism: 2,
		SaltLength:  16,
		KeyLength:   32,
	}
}

// PasswordHasher creates and verifies Argon2id PHC password hashes.
type PasswordHasher struct {
	Params PasswordParams
	Rand   io.Reader
}

// Hash hashes a password after applying the carpool password policy.
func (h PasswordHasher) Hash(password string) (string, error) {
	if errValidate := ValidatePassword(password); errValidate != nil {
		return "", errValidate
	}
	params := h.Params
	if errParams := params.validate(); errParams != nil {
		return "", errParams
	}
	random := h.Rand
	if random == nil {
		random = rand.Reader
	}
	salt := make([]byte, params.SaltLength)
	if _, errRead := io.ReadFull(random, salt); errRead != nil {
		return "", fmt.Errorf("carpool: generate password salt: %w", errRead)
	}
	digest := argon2.IDKey([]byte(password), salt, params.Iterations, params.Memory, params.Parallelism, params.KeyLength)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s",
		argon2.Version,
		params.Memory,
		params.Iterations,
		params.Parallelism,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(digest),
	), nil
}

// Verify verifies a password without including either secret in returned errors.
func (h PasswordHasher) Verify(encodedHash, password string) (bool, error) {
	params, salt, expected, errParse := parsePasswordHash(encodedHash)
	if errParse != nil {
		return false, errParse
	}
	actual := argon2.IDKey([]byte(password), salt, params.Iterations, params.Memory, params.Parallelism, uint32(len(expected)))
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

// NeedsRehash reports whether an encoded password uses different parameters.
func (h PasswordHasher) NeedsRehash(encodedHash string) bool {
	params, _, _, errParse := parsePasswordHash(encodedHash)
	if errParse != nil {
		return true
	}
	return params != h.Params
}

// NormalizeUsername validates and normalizes a login identifier.
func NormalizeUsername(username string) (string, error) {
	normalized := strings.ToLower(strings.TrimSpace(username))
	if !usernamePattern.MatchString(normalized) {
		return "", errors.New("carpool: username must be 3-64 ASCII letters, digits, dots, underscores, or hyphens")
	}
	return normalized, nil
}

// NormalizeDisplayName validates and NFC-normalizes a public car display name.
func NormalizeDisplayName(displayName string) (string, string, error) {
	normalized := norm.NFC.String(strings.TrimSpace(displayName))
	runeCount := utf8.RuneCountInString(normalized)
	if runeCount < 1 || runeCount > 64 {
		return "", "", errors.New("carpool: display name must contain 1-64 characters")
	}
	for _, value := range normalized {
		if unicode.IsControl(value) {
			return "", "", errors.New("carpool: display name cannot contain control characters")
		}
	}
	return normalized, strings.ToLower(normalized), nil
}

// ValidatePassword applies the first-phase password length policy.
func ValidatePassword(password string) error {
	if !utf8.ValidString(password) {
		return errors.New("carpool: password must be valid UTF-8")
	}
	length := utf8.RuneCountInString(password)
	if length < 12 || length > 128 {
		return errors.New("carpool: password must contain 12-128 characters")
	}
	return nil
}

// SessionSecret contains a browser session bearer token and its persisted digest.
type SessionSecret struct {
	Token     string
	Digest    []byte
	CSRFToken string
}

// NewSessionSecret creates independent session and CSRF values.
func NewSessionSecret(random io.Reader) (SessionSecret, error) {
	if random == nil {
		random = rand.Reader
	}
	tokenSecret, errToken := randomURLSecret(random, 32)
	if errToken != nil {
		return SessionSecret{}, fmt.Errorf("carpool: generate session token: %w", errToken)
	}
	csrfToken, errCSRF := randomURLSecret(random, 32)
	if errCSRF != nil {
		return SessionSecret{}, fmt.Errorf("carpool: generate CSRF token: %w", errCSRF)
	}
	token := sessionPrefix + tokenSecret
	digest := sha256.Sum256([]byte(token))
	return SessionSecret{Token: token, Digest: digest[:], CSRFToken: csrfToken}, nil
}

// SessionTokenDigest validates a browser session token and returns its lookup digest.
func SessionTokenDigest(token string) ([]byte, error) {
	if !strings.HasPrefix(token, sessionPrefix) {
		return nil, errors.New("carpool: unsupported session token prefix")
	}
	secret := strings.TrimPrefix(token, sessionPrefix)
	if !validURLSecret(secret, 32) {
		return nil, errors.New("carpool: invalid session token format")
	}
	digest := sha256.Sum256([]byte(token))
	return digest[:], nil
}

// UserAPIKeySecret contains a one-time user key and persisted lookup material.
type UserAPIKeySecret struct {
	KeyID  string
	Secret string
	Token  string
	Digest []byte
}

// NewUserAPIKeySecret creates a versioned high-entropy user API key.
func NewUserAPIKeySecret(random io.Reader) (UserAPIKeySecret, error) {
	if random == nil {
		random = rand.Reader
	}
	keyID, errID := randomURLSecret(random, 12)
	if errID != nil {
		return UserAPIKeySecret{}, fmt.Errorf("carpool: generate API key ID: %w", errID)
	}
	secret, errSecret := randomURLSecret(random, 32)
	if errSecret != nil {
		return UserAPIKeySecret{}, fmt.Errorf("carpool: generate API key secret: %w", errSecret)
	}
	token := UserAPIKeyPrefix + keyID + "." + secret
	digest := sha256.Sum256([]byte(secret))
	return UserAPIKeySecret{KeyID: keyID, Secret: secret, Token: token, Digest: digest[:]}, nil
}

// ParseUserAPIKey extracts lookup material and a digest without retaining the full token.
func ParseUserAPIKey(token string) (string, []byte, error) {
	if !strings.HasPrefix(token, UserAPIKeyPrefix) {
		return "", nil, errors.New("carpool: unsupported API key prefix")
	}
	remainder := strings.TrimPrefix(token, UserAPIKeyPrefix)
	parts := strings.Split(remainder, ".")
	if len(parts) != 2 || !validURLSecret(parts[0], 12) || !validURLSecret(parts[1], 32) {
		return "", nil, errors.New("carpool: invalid API key format")
	}
	digest := sha256.Sum256([]byte(parts[1]))
	return parts[0], digest[:], nil
}

// SecretDigestEqual performs a constant-time digest comparison.
func SecretDigestEqual(left, right []byte) bool {
	return len(left) == sha256.Size && len(right) == sha256.Size && subtle.ConstantTimeCompare(left, right) == 1
}

func (p PasswordParams) validate() error {
	if p.Memory < 8 || p.Memory > 1024*1024 ||
		p.Iterations == 0 || p.Iterations > 20 ||
		p.Parallelism == 0 || p.Parallelism > 32 ||
		p.SaltLength < 16 || p.SaltLength > 64 ||
		p.KeyLength < 16 || p.KeyLength > 64 {
		return errors.New("carpool: invalid Argon2id parameters")
	}
	return nil
}

func parsePasswordHash(encoded string) (PasswordParams, []byte, []byte, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return PasswordParams{}, nil, nil, errors.New("carpool: invalid password hash")
	}
	version, errVersion := strconv.Atoi(strings.TrimPrefix(parts[2], "v="))
	if errVersion != nil || version != argon2.Version {
		return PasswordParams{}, nil, nil, errors.New("carpool: unsupported password hash version")
	}
	var params PasswordParams
	if _, errScan := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &params.Memory, &params.Iterations, &params.Parallelism); errScan != nil {
		return PasswordParams{}, nil, nil, errors.New("carpool: invalid password hash parameters")
	}
	salt, errSalt := base64.RawStdEncoding.Strict().DecodeString(parts[4])
	if errSalt != nil {
		return PasswordParams{}, nil, nil, errors.New("carpool: invalid password hash salt")
	}
	digest, errDigest := base64.RawStdEncoding.Strict().DecodeString(parts[5])
	if errDigest != nil {
		return PasswordParams{}, nil, nil, errors.New("carpool: invalid password hash digest")
	}
	params.SaltLength = uint32(len(salt))
	params.KeyLength = uint32(len(digest))
	if errParams := params.validate(); errParams != nil {
		return PasswordParams{}, nil, nil, errParams
	}
	return params, salt, digest, nil
}

func randomURLSecret(random io.Reader, size int) (string, error) {
	raw := make([]byte, size)
	if _, errRead := io.ReadFull(random, raw); errRead != nil {
		return "", errRead
	}
	return base64.RawURLEncoding.EncodeToString(raw), nil
}

func validURLSecret(value string, rawSize int) bool {
	decoded, errDecode := base64.RawURLEncoding.Strict().DecodeString(value)
	return errDecode == nil && len(decoded) == rawSize
}
