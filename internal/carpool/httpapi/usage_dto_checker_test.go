package httpapi

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestCheckerUsageDTOExcludesInternalCredentialAndScopeFields(t *testing.T) {
	const private = "checker-private-canary"
	zero := int64(0)
	item := domain.UsageRequestDetail{
		Request: domain.ProxyRequest{RequestID: "request-public", UserID: private, CarID: private, MembershipID: private, ScopeHash: private, PricingCatalogHash: private, BilledNanoUSD: &zero},
		UserRef: "usr_public", CarRef: "car_public", APIKeyRef: "public-key-id", EventCount: 1, UnknownEvents: 1,
		Events: []domain.UsageEvent{{EventID: "event-public", AuthID: private, AssignmentID: private, BillingPeriodID: private, PriceInputPerToken: private, PriceOutputPerToken: private, AccountRefSnapshot: "acct_public", SafeLabelSnapshot: "Safe label", PricingStatus: "unknown", CacheReadTokens: &zero}},
	}
	for _, includeEvents := range []bool{false, true} {
		response := usageRequestResponse(item, includeEvents)
		encoded, errMarshal := json.Marshal(response)
		if errMarshal != nil {
			t.Fatal(errMarshal)
		}
		if strings.Contains(string(encoded), private) {
			t.Fatalf("internal fields leaked in DTO: %s", encoded)
		}
		if _, exists := response["events"]; exists != includeEvents {
			t.Fatalf("event expansion = %v, want %v", exists, includeEvents)
		}
		if response["billed_usd"] != "0" || response["unknown_cost_events"] != int64(1) {
			t.Fatalf("known zero subtotal or unknown marker lost: %+v", response)
		}
	}
	event := usageEventResponse(item.Events[0])
	if event["usage_known"] != false || event["cost_usd"] != nil || event["input_tokens"] != nil || event["cache_read_tokens"] != int64(0) || event["cache_write_tokens"] != nil {
		t.Fatalf("unknown and zero fields conflated: %+v", event)
	}
}

func TestCheckerRetentionDTOExcludesStoredConfirmation(t *testing.T) {
	response := retentionJobResponse(domain.RetentionJob{ID: "job-public", Operation: "usage_details", Confirmation: "checker-private-target-state", Status: "queued"})
	encoded, errMarshal := json.Marshal(response)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	if strings.Contains(string(encoded), "checker-private") || strings.Contains(string(encoded), "\"confirmation\"") {
		t.Fatalf("server-only confirmation leaked: %s", encoded)
	}
}
