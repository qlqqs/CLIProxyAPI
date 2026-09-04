package service

import (
	"bytes"
	"strings"
	"testing"
)

func testPasswordParams() PasswordParams {
	return PasswordParams{Memory: 64, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 32}
}

func TestPasswordHasherRoundTrip(t *testing.T) {
	hasher := PasswordHasher{Params: testPasswordParams(), Rand: bytes.NewReader(bytes.Repeat([]byte{7}, 16))}
	encoded, errHash := hasher.Hash("correct horse battery")
	if errHash != nil {
		t.Fatalf("Hash() error = %v", errHash)
	}
	if strings.Contains(encoded, "correct horse battery") {
		t.Fatal("encoded hash contains plaintext password")
	}
	matched, errVerify := hasher.Verify(encoded, "correct horse battery")
	if errVerify != nil || !matched {
		t.Fatalf("Verify(correct) = (%t, %v)", matched, errVerify)
	}
	matched, errVerify = hasher.Verify(encoded, "wrong password value")
	if errVerify != nil || matched {
		t.Fatalf("Verify(wrong) = (%t, %v)", matched, errVerify)
	}
	if hasher.NeedsRehash(encoded) {
		t.Fatal("NeedsRehash() = true for matching parameters")
	}
	productionHasher := PasswordHasher{Params: DefaultPasswordParams()}
	if !productionHasher.NeedsRehash(encoded) {
		t.Fatal("NeedsRehash() = false for different parameters")
	}
}

func TestNormalizeUsername(t *testing.T) {
	got, errNormalize := NormalizeUsername("  Alice.Example  ")
	if errNormalize != nil || got != "alice.example" {
		t.Fatalf("NormalizeUsername() = (%q, %v)", got, errNormalize)
	}
	for _, invalid := range []string{"ab", "bad name", "用户", "-leading", "trailing-"} {
		if _, errInvalid := NormalizeUsername(invalid); errInvalid == nil {
			t.Fatalf("NormalizeUsername(%q) accepted invalid value", invalid)
		}
	}
}

func TestNormalizeDisplayName(t *testing.T) {
	got, key, errNormalize := NormalizeDisplayName("  Alice  ")
	if errNormalize != nil || got != "Alice" || key != "alice" {
		t.Fatalf("NormalizeDisplayName() = (%q, %q, %v)", got, key, errNormalize)
	}
	if _, _, errControl := NormalizeDisplayName("bad\nname"); errControl == nil {
		t.Fatal("NormalizeDisplayName() accepted control character")
	}
}

func TestSessionSecretUsesIndependentValues(t *testing.T) {
	randomBytes := append(bytes.Repeat([]byte{3}, 32), bytes.Repeat([]byte{4}, 32)...)
	random := bytes.NewReader(randomBytes)
	secret, errCreate := NewSessionSecret(random)
	if errCreate != nil {
		t.Fatalf("NewSessionSecret() error = %v", errCreate)
	}
	if !strings.HasPrefix(secret.Token, sessionPrefix) || len(secret.Digest) != 32 {
		t.Fatalf("session secret = %#v", secret)
	}
	if secret.CSRFToken == "" || strings.Contains(secret.Token, secret.CSRFToken) {
		t.Fatal("session and CSRF values are not independent")
	}
	parsedDigest, errDigest := SessionTokenDigest(secret.Token)
	if errDigest != nil {
		t.Fatalf("SessionTokenDigest() error = %v", errDigest)
	}
	if !SecretDigestEqual(parsedDigest, secret.Digest) {
		t.Fatal("SessionTokenDigest() did not reproduce persisted digest")
	}
}

func TestSessionTokenDigestRejectsMalformedTokens(t *testing.T) {
	for _, token := range []string{"", "wrong_prefix", "cps_v1_short", "cps_v1_!!!"} {
		if _, errDigest := SessionTokenDigest(token); errDigest == nil {
			t.Fatalf("SessionTokenDigest(%q) unexpectedly succeeded", token)
		}
	}
}

func TestUserAPIKeyRoundTrip(t *testing.T) {
	random := bytes.NewReader(bytes.Repeat([]byte{9}, 44))
	secret, errCreate := NewUserAPIKeySecret(random)
	if errCreate != nil {
		t.Fatalf("NewUserAPIKeySecret() error = %v", errCreate)
	}
	keyID, digest, errParse := ParseUserAPIKey(secret.Token)
	if errParse != nil {
		t.Fatalf("ParseUserAPIKey() error = %v", errParse)
	}
	if keyID != secret.KeyID || !SecretDigestEqual(digest, secret.Digest) {
		t.Fatalf("parsed API key lookup does not match generated material")
	}
	if _, _, errInvalid := ParseUserAPIKey("legacy-key"); errInvalid == nil {
		t.Fatal("ParseUserAPIKey() accepted a legacy key")
	}
}

func TestValidatePassword(t *testing.T) {
	if errValid := ValidatePassword("twelve-chars!"); errValid != nil {
		t.Fatalf("ValidatePassword(valid) error = %v", errValid)
	}
	if errShort := ValidatePassword("too-short"); errShort == nil {
		t.Fatal("ValidatePassword(short) accepted value")
	}
	if errLong := ValidatePassword(strings.Repeat("a", 129)); errLong == nil {
		t.Fatal("ValidatePassword(long) accepted value")
	}
}
