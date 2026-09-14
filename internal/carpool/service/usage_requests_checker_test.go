package service

import (
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestCheckerUsageCursorRevalidatesChangedRetentionPolicy(t *testing.T) {
	f := newUsageRequestsFixture(t)
	days := int64(180)
	if _, errSettings := f.store.SetRetentionOverride(t.Context(), &days, nil); errSettings != nil {
		t.Fatal(errSettings)
	}
	from, to := f.now.Add(-150*24*time.Hour), *f.now
	for i := range 26 {
		f.seed(t, fmt.Sprintf("checker-request-%02d", i), f.now.Add(-120*24*time.Hour))
	}
	query := UsageRequestQuery{From: &from, To: &to}
	first, errFirst := f.control.AdminUsageRequests(t.Context(), f.admin, query)
	if errFirst != nil || len(first.Items) != 25 || first.NextCursor == "" {
		t.Fatalf("first page = %+v, %v", first, errFirst)
	}
	query.Cursor = first.NextCursor
	days = 90
	if _, errSettings := f.store.SetRetentionOverride(t.Context(), &days, nil); errSettings != nil {
		t.Fatal(errSettings)
	}
	if _, errPage := f.control.AdminUsageRequests(t.Context(), f.admin, query); !errors.Is(errPage, domain.ErrInvalid) {
		t.Fatalf("cursor bypassed changed retention policy: %v", errPage)
	}
	days = 0
	if _, errSettings := f.store.SetRetentionOverride(t.Context(), &days, nil); errSettings != nil {
		t.Fatal(errSettings)
	}
	second, errSecond := f.control.AdminUsageRequests(t.Context(), f.admin, query)
	if errSecond != nil || len(second.Items) != 1 || second.Period != first.Period {
		t.Fatalf("permanent policy did not preserve frozen range: %+v, %v", second, errSecond)
	}
}
