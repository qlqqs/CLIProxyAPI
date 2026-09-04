package service

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestNewPublicRefAndTemporaryPassword(t *testing.T) {
	ref, errRef := NewPublicRef(bytes.NewReader(bytes.Repeat([]byte{1}, 12)), "usr")
	if errRef != nil || !strings.HasPrefix(ref, "usr_") {
		t.Fatalf("NewPublicRef() = %q, %v", ref, errRef)
	}
	password, errPassword := NewTemporaryPassword(bytes.NewReader(bytes.Repeat([]byte{2}, temporaryPasswordBytes)))
	if errPassword != nil {
		t.Fatalf("NewTemporaryPassword() error = %v", errPassword)
	}
	if errValidate := ValidatePassword(password); errValidate != nil {
		t.Fatalf("generated password failed validation: %v", errValidate)
	}
	if _, errRef = NewPublicRef(nil, "bad prefix"); errRef == nil {
		t.Fatal("NewPublicRef() accepted invalid prefix")
	}
}

func TestResolveReportPeriodUsesConfiguredTimezone(t *testing.T) {
	location, errLocation := time.LoadLocation("Asia/Shanghai")
	if errLocation != nil {
		t.Fatalf("LoadLocation() error = %v", errLocation)
	}
	now := time.Date(2026, 9, 4, 1, 30, 0, 0, time.UTC)
	today, errToday := ResolveReportPeriod("today", now, location)
	if errToday != nil {
		t.Fatalf("ResolveReportPeriod(today) error = %v", errToday)
	}
	wantStart := time.Date(2026, 9, 3, 16, 0, 0, 0, time.UTC)
	if !today.From.Equal(wantStart) || !today.To.Equal(now) {
		t.Fatalf("today period = [%s, %s), want [%s, %s)", today.From, today.To, wantStart, now)
	}

	seven, errSeven := ResolveReportPeriod("7d", now, location)
	if errSeven != nil || !seven.From.Equal(now.AddDate(0, 0, -7)) || !seven.To.Equal(now) {
		t.Fatalf("7d period = %#v, %v", seven, errSeven)
	}
	if _, errInvalid := ResolveReportPeriod("week", now, location); errInvalid == nil {
		t.Fatal("ResolveReportPeriod() accepted unsupported period")
	}
}

func TestValidateCustomReportPeriod(t *testing.T) {
	cutoff := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	from := cutoff.Add(time.Hour)
	to := from.Add(24 * time.Hour)
	period, errPeriod := ValidateCustomReportPeriod(from, to, cutoff)
	if errPeriod != nil || !period.From.Equal(from) || !period.To.Equal(to) {
		t.Fatalf("ValidateCustomReportPeriod() = %#v, %v", period, errPeriod)
	}
	if _, errOld := ValidateCustomReportPeriod(cutoff.Add(-time.Second), to, cutoff); errOld == nil {
		t.Fatal("ValidateCustomReportPeriod() accepted data before retention cutoff")
	}
	if _, errReverse := ValidateCustomReportPeriod(to, from, cutoff); errReverse == nil {
		t.Fatal("ValidateCustomReportPeriod() accepted reverse interval")
	}
}
