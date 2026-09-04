package service

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

const (
	temporaryPasswordBytes = 24
	pageCursorVersion      = 1
	maximumPageCursorBytes = 1024
)

type pageCursor struct {
	Version int    `json:"v"`
	Kind    string `json:"kind"`
	Value   string `json:"value"`
	Time    string `json:"time,omitempty"`
}

// ReportPeriod is a server-computed UTC half-open reporting interval.
type ReportPeriod struct {
	Name string
	From time.Time
	To   time.Time
}

// NewPublicRef creates a non-secret stable reference with a fixed type prefix.
func NewPublicRef(random io.Reader, prefix string) (string, error) {
	prefix = strings.TrimSpace(prefix)
	if prefix == "" || strings.ContainsAny(prefix, " .:/\\") {
		return "", errors.New("carpool: invalid public reference prefix")
	}
	if random == nil {
		random = rand.Reader
	}
	value, errValue := randomURLSecret(random, 12)
	if errValue != nil {
		return "", fmt.Errorf("carpool: generate public reference: %w", errValue)
	}
	return prefix + "_" + value, nil
}

// NewTemporaryPassword creates a policy-compliant one-time password.
func NewTemporaryPassword(random io.Reader) (string, error) {
	if random == nil {
		random = rand.Reader
	}
	raw := make([]byte, temporaryPasswordBytes)
	if _, errRead := io.ReadFull(random, raw); errRead != nil {
		return "", fmt.Errorf("carpool: generate temporary password: %w", errRead)
	}
	password := base64.RawURLEncoding.EncodeToString(raw)
	if errValidate := ValidatePassword(password); errValidate != nil {
		return "", fmt.Errorf("carpool: validate generated temporary password: %w", errValidate)
	}
	return password, nil
}

// ResolveReportPeriod computes a supported passenger reporting interval.
func ResolveReportPeriod(name string, now time.Time, location *time.Location) (ReportPeriod, error) {
	if location == nil {
		location = time.UTC
	}
	now = now.UTC()
	switch strings.TrimSpace(name) {
	case "today":
		localNow := now.In(location)
		localStart := time.Date(localNow.Year(), localNow.Month(), localNow.Day(), 0, 0, 0, 0, location)
		return ReportPeriod{Name: "today", From: localStart.UTC(), To: now}, nil
	case "7d":
		return ReportPeriod{Name: "7d", From: now.AddDate(0, 0, -7), To: now}, nil
	case "30d":
		return ReportPeriod{Name: "30d", From: now.AddDate(0, 0, -30), To: now}, nil
	default:
		return ReportPeriod{}, fmt.Errorf("carpool: unsupported report period: %w", domain.ErrInvalid)
	}
}

// ValidateCustomReportPeriod validates an administrator-supplied UTC interval.
func ValidateCustomReportPeriod(from, to, retentionCutoff time.Time) (ReportPeriod, error) {
	from = from.UTC()
	to = to.UTC()
	retentionCutoff = retentionCutoff.UTC()
	if from.IsZero() || to.IsZero() || !from.Before(to) {
		return ReportPeriod{}, fmt.Errorf("carpool: invalid report interval: %w", domain.ErrInvalid)
	}
	if from.Before(retentionCutoff) {
		return ReportPeriod{}, fmt.Errorf("carpool: report interval precedes retention cutoff: %w", domain.ErrInvalid)
	}
	return ReportPeriod{Name: "custom", From: from, To: to}, nil
}

func encodePageCursor(kind, value string) string {
	payload, _ := json.Marshal(pageCursor{Version: pageCursorVersion, Kind: kind, Value: value})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodePageCursor(encoded, kind string) (string, error) {
	if strings.TrimSpace(encoded) == "" {
		return "", nil
	}
	payload, errDecode := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if errDecode != nil || len(payload) == 0 || len(payload) > maximumPageCursorBytes {
		return "", fmt.Errorf("carpool: invalid page cursor: %w", domain.ErrInvalid)
	}
	var cursor pageCursor
	if errJSON := json.Unmarshal(payload, &cursor); errJSON != nil || cursor.Version != pageCursorVersion || cursor.Kind != kind || !validCursorValue(cursor.Value) || cursor.Time != "" {
		return "", fmt.Errorf("carpool: invalid page cursor: %w", domain.ErrInvalid)
	}
	return cursor.Value, nil
}

func encodeAuditCursor(before time.Time, beforeID string) string {
	payload, _ := json.Marshal(pageCursor{
		Version: pageCursorVersion,
		Kind:    "audit",
		Value:   beforeID,
		Time:    before.UTC().Format(time.RFC3339Nano),
	})
	return base64.RawURLEncoding.EncodeToString(payload)
}

func decodeAuditCursor(encoded string) (time.Time, string, error) {
	if strings.TrimSpace(encoded) == "" {
		return time.Time{}, "", nil
	}
	payload, errDecode := base64.RawURLEncoding.Strict().DecodeString(encoded)
	if errDecode != nil || len(payload) == 0 || len(payload) > maximumPageCursorBytes {
		return time.Time{}, "", fmt.Errorf("carpool: invalid audit cursor: %w", domain.ErrInvalid)
	}
	var cursor pageCursor
	if errJSON := json.Unmarshal(payload, &cursor); errJSON != nil || cursor.Version != pageCursorVersion || cursor.Kind != "audit" || !validCursorValue(cursor.Value) || cursor.Time == "" {
		return time.Time{}, "", fmt.Errorf("carpool: invalid audit cursor: %w", domain.ErrInvalid)
	}
	before, errTime := time.Parse(time.RFC3339Nano, cursor.Time)
	if errTime != nil || before.IsZero() {
		return time.Time{}, "", fmt.Errorf("carpool: invalid audit cursor: %w", domain.ErrInvalid)
	}
	return before.UTC(), cursor.Value, nil
}

func validatePublicRef(value, prefix string) error {
	value = strings.TrimSpace(value)
	marker := prefix + "_"
	if !strings.HasPrefix(value, marker) || !validURLSecret(strings.TrimPrefix(value, marker), 12) {
		return fmt.Errorf("carpool: invalid %s reference: %w", prefix, domain.ErrInvalid)
	}
	return nil
}

func validCursorValue(value string) bool {
	if value == "" || len(value) > 256 {
		return false
	}
	for _, char := range value {
		if char < 0x21 || char > 0x7e {
			return false
		}
	}
	return true
}
