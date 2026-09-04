package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	log "github.com/sirupsen/logrus"
)

func TestLoginRejectionAuditUsesSafeTargetAndLogsPersistenceFailure(t *testing.T) {
	now := time.Date(2026, time.September, 4, 18, 0, 0, 0, time.UTC)
	repository := &loginAuditRepository{insertErr: errors.New("audit unavailable")}
	limiter := NewLoginLimiter(LoginLimiterConfig{
		Burst:          1,
		RefillInterval: time.Hour,
		Now:            func() time.Time { return now },
	})
	control, errControl := NewControl(repository, nil, ControlConfig{
		SessionAbsoluteTTL: 24 * time.Hour,
		SessionIdleTTL:     2 * time.Hour,
		UsageRetention:     90 * 24 * time.Hour,
		Now:                func() time.Time { return now },
		Random:             bytes.NewReader(bytes.Repeat([]byte{1}, 64)),
		PasswordHasher: PasswordHasher{
			Params: testPasswordParams(),
			Rand:   bytes.NewReader(bytes.Repeat([]byte{2}, 16)),
		},
		LoginLimiter:    limiter,
		CandidateRefKey: bytes.Repeat([]byte{3}, 32),
	})
	if errControl != nil {
		t.Fatalf("NewControl() error = %v", errControl)
	}

	hook := installAuditLogHook(t)
	username := "sensitive-user"
	password := "sensitive-password"
	clientAddress := "192.0.2.45"
	if _, errLogin := control.Login(t.Context(), username, password, clientAddress); !errors.Is(errLogin, ErrUnauthenticated) {
		t.Fatalf("Login(first) error = %v, want ErrUnauthenticated", errLogin)
	}
	if _, errLogin := control.Login(t.Context(), username, password, clientAddress); !errors.Is(errLogin, ErrRateLimited) {
		t.Fatalf("Login(second) error = %v, want ErrRateLimited", errLogin)
	}

	if len(repository.events) != 2 {
		t.Fatalf("audit event count = %d, want 2", len(repository.events))
	}
	for _, event := range repository.events {
		if event.TargetRef == "" || !strings.HasPrefix(event.TargetRef, "login_") {
			t.Fatalf("audit target_ref = %q, want a non-empty safe login reference", event.TargetRef)
		}
		for _, secret := range []string{username, password, clientAddress} {
			if strings.Contains(event.TargetRef, secret) {
				t.Fatalf("audit target_ref contains sensitive input %q", secret)
			}
		}
	}

	if len(hook.entries) != 2 {
		t.Fatalf("audit persistence log count = %d, want 2", len(hook.entries))
	}
	for _, entry := range hook.entries {
		if entry.Message != "carpool audit event was not persisted" || entry.Data["action"] != "login" || entry.Data["reason"] != "audit_persist_failed" {
			t.Fatalf("audit persistence log = %#v", entry)
		}
		serialized := entry.Message + " " + fieldsString(entry.Data)
		for _, secret := range []string{username, password, clientAddress} {
			if strings.Contains(serialized, secret) {
				t.Fatalf("audit persistence log contains sensitive input %q", secret)
			}
		}
	}
}

type loginAuditRepository struct {
	Repository
	events    []domain.AuditEvent
	insertErr error
}

func (r *loginAuditRepository) GetUserByNormalizedUsername(context.Context, string) (domain.User, error) {
	return domain.User{}, domain.ErrNotFound
}

func (r *loginAuditRepository) InsertAuditEvent(_ context.Context, event domain.AuditEvent) (domain.AuditEvent, error) {
	r.events = append(r.events, event)
	return domain.AuditEvent{}, r.insertErr
}

type auditLogHook struct {
	entries []*log.Entry
}

func (h *auditLogHook) Levels() []log.Level { return log.AllLevels }

func (h *auditLogHook) Fire(entry *log.Entry) error {
	copyEntry := *entry
	copyEntry.Data = make(log.Fields, len(entry.Data))
	for key, value := range entry.Data {
		copyEntry.Data[key] = value
	}
	h.entries = append(h.entries, &copyEntry)
	return nil
}

func installAuditLogHook(t *testing.T) *auditLogHook {
	t.Helper()
	logger := log.StandardLogger()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	oldOutput := logger.Out
	logger.SetOutput(io.Discard)
	t.Cleanup(func() {
		logger.ReplaceHooks(oldHooks)
		logger.SetOutput(oldOutput)
	})
	hook := &auditLogHook{}
	logger.AddHook(hook)
	return hook
}

func fieldsString(fields log.Fields) string {
	var builder strings.Builder
	for key, value := range fields {
		builder.WriteString(key)
		builder.WriteString("=")
		builder.WriteString(value.(string))
		builder.WriteString(" ")
	}
	return builder.String()
}
