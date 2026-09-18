package httpapi

import (
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const maxAccountImportBatch = 100

var errAccountImport = errors.New("invalid account import")

type normalizedAccountImport struct {
	name string
	body []byte
}

type sub2APIAccount struct {
	Name        string `json:"name"`
	Platform    string `json:"platform"`
	Type        string `json:"type"`
	Credentials *struct {
		AccessToken      string `json:"access_token"`
		RefreshToken     string `json:"refresh_token"`
		IDToken          string `json:"id_token"`
		ChatGPTAccountID string `json:"chatgpt_account_id"`
		AccountID        string `json:"account_id"`
		Email            string `json:"email"`
		ExpiresAt        int64  `json:"expires_at"`
		ExpiresIn        int64  `json:"expires_in"`
		OrganizationID   string `json:"organization_id"`
		PlanType         string `json:"plan_type"`
	} `json:"credentials"`
	Extra *struct {
		Email             string `json:"email"`
		DisplayName       string `json:"display_name"`
		OpenAIPassthrough bool   `json:"openai_passthrough"`
		Recovery          *struct {
			Email          string `json:"email"`
			LoginPassword  string `json:"login_password"`
			TOTPSecret     string `json:"totp_secret"`
			CredentialLine string `json:"credential_line"`
		} `json:"recovery"`
	} `json:"extra"`
	Concurrency        int     `json:"concurrency"`
	Priority           int     `json:"priority"`
	RateMultiplier     float64 `json:"rate_multiplier"`
	AutoPauseOnExpired bool    `json:"auto_pause_on_expired"`
	PlanType           string  `json:"plan_type"`
}

// normalizeSub2API validates the entire export before producing any write work.
// Only explicitly constructed credential fields can reach the original uploader.
func normalizeSub2API(name string, body []byte) ([]normalizedAccountImport, error) {
	if !safeAccountFileName(name) {
		return nil, errAccountImport
	}
	var export struct {
		Type       string            `json:"type"`
		Version    int               `json:"version"`
		ExportedAt string            `json:"exported_at"`
		Proxies    []json.RawMessage `json:"proxies"`
		Accounts   []*sub2APIAccount `json:"accounts"`
	}
	if json.Unmarshal(body, &export) != nil || export.Type != "sub2api-data" || export.Version != 1 || len(export.Accounts) == 0 || len(export.Accounts) > maxAccountImportBatch {
		return nil, errAccountImport
	}
	// Explicit null is not a valid typed field in this export schema. Unknown
	// fields are ignored rather than copied, including future secret fields.
	if !validImportFieldTypes(body, sub2APIImportShape) {
		return nil, errAccountImport
	}
	files := make([]normalizedAccountImport, 0, len(export.Accounts))
	for i, account := range export.Accounts {
		if account == nil || account.Platform != "openai" || account.Type != "oauth" || account.Credentials == nil || strings.TrimSpace(account.Credentials.AccessToken) == "" {
			return nil, errAccountImport
		}
		credentials := account.Credentials
		metadata := map[string]string{"type": "codex", "access_token": credentials.AccessToken}
		for key, value := range map[string]string{"refresh_token": credentials.RefreshToken, "id_token": credentials.IDToken} {
			if value != "" {
				metadata[key] = value
			}
		}
		accountID := credentials.ChatGPTAccountID
		if accountID == "" {
			accountID = credentials.AccountID
		}
		if accountID != "" {
			metadata["account_id"] = accountID
		}
		email := credentials.Email
		if account.Extra != nil && account.Extra.Email != "" {
			email = account.Extra.Email
		}
		if email != "" {
			metadata["email"] = email
		}
		if credentials.ExpiresAt > 0 {
			expiration := time.Unix(credentials.ExpiresAt, 0).UTC()
			if expiration.Year() > 9999 {
				return nil, errAccountImport
			}
			metadata["expired"] = expiration.Format(time.RFC3339)
		}
		plan := credentials.PlanType
		if plan == "" {
			plan = account.PlanType
		}
		switch plan {
		case "free", "plus", "pro", "team", "business", "enterprise", "edu":
			metadata["plan_type"] = plan
		}
		filename := name
		if len(export.Accounts) > 1 {
			stem := name[:len(name)-len(".json")]
			filename = fmt.Sprintf("%s-%03d.json", stem, i+1)
			if !safeAccountFileName(filename) {
				digest := sha256.Sum256([]byte(name))
				filename = fmt.Sprintf("import-%x-%03d.json", digest[:16], i+1)
			}
		}
		encoded, errMarshal := json.Marshal(metadata)
		if errMarshal != nil {
			return nil, errAccountImport
		}
		files = append(files, normalizedAccountImport{name: filename, body: encoded})
	}
	return files, nil
}

// The typed decoder checks scalar types; this shape also rejects explicit null
// for known fields and traverses the nested objects without retaining secrets.
var sub2APIImportShape = map[string]any{
	"type": nil, "version": nil, "exported_at": nil, "proxies": nil,
	"accounts": []any{map[string]any{
		"name": nil, "platform": nil, "type": nil, "concurrency": nil, "priority": nil, "rate_multiplier": nil, "auto_pause_on_expired": nil, "plan_type": nil,
		"credentials": map[string]any{"access_token": nil, "refresh_token": nil, "id_token": nil, "chatgpt_account_id": nil, "account_id": nil, "email": nil, "expires_at": nil, "expires_in": nil, "organization_id": nil, "plan_type": nil},
		"extra":       map[string]any{"email": nil, "display_name": nil, "openai_passthrough": nil, "recovery": map[string]any{"email": nil, "login_password": nil, "totp_secret": nil, "credential_line": nil}},
	}},
}

func validImportFieldTypes(data json.RawMessage, shape any) bool {
	if strings.TrimSpace(string(data)) == "null" {
		return false
	}
	switch schema := shape.(type) {
	case map[string]any:
		var fields map[string]json.RawMessage
		if json.Unmarshal(data, &fields) != nil || fields == nil {
			return false
		}
		for key, child := range schema {
			if value, exists := fields[key]; exists && !validImportFieldTypes(value, child) {
				return false
			}
		}
	case []any:
		var items []json.RawMessage
		if json.Unmarshal(data, &items) != nil {
			return false
		}
		for _, item := range items {
			if !validImportFieldTypes(item, schema[0]) {
				return false
			}
		}
	}
	return true
}
