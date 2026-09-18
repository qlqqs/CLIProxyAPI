package httpapi

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	management "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

const syntheticSub2Account = `{"name":"DO_NOT_USE_FOR_PATH","platform":"openai","type":"oauth","credentials":{"access_token":"dummy-access","refresh_token":"dummy-refresh","chatgpt_account_id":"dummy-account","expires_at":1700000000,"expires_in":3600,"organization_id":"DROP_ORG","plan_type":"plus"},"extra":{"email":"dummy@example.test","display_name":"DROP_DISPLAY","openai_passthrough":true,"recovery":{"email":"DROP_EMAIL","login_password":"DROP_PASSWORD","totp_secret":"DROP_TOTP","credential_line":"DROP_LINE"}},"concurrency":3,"priority":1,"rate_multiplier":1,"auto_pause_on_expired":true}`

func syntheticSub2Export(accounts ...string) string {
	return `{"type":"sub2api-data","version":1,"exported_at":"synthetic","proxies":[],"accounts":[` + strings.Join(accounts, ",") + `]}`
}

func TestNormalizeSub2API(t *testing.T) {
	fractional := strings.Replace(syntheticSub2Account, `"rate_multiplier":1`, `"rate_multiplier":0.5`, 1)
	files, err := normalizeSub2API("upload.json", []byte(syntheticSub2Export(fractional)))
	if err != nil || len(files) != 1 || files[0].name != "upload.json" {
		t.Fatal("single normalization failed")
	}
	var metadata map[string]string
	if errDecode := json.Unmarshal(files[0].body, &metadata); errDecode != nil {
		t.Fatal(errDecode)
	}
	want := map[string]string{"type": "codex", "access_token": "dummy-access", "refresh_token": "dummy-refresh", "account_id": "dummy-account", "email": "dummy@example.test", "expired": "2023-11-14T22:13:20Z", "plan_type": "plus"}
	if len(metadata) != len(want) {
		t.Fatal("unexpected persisted fields")
	}
	for key, value := range want {
		if metadata[key] != value {
			t.Errorf("incorrect mapping: %s", key)
		}
	}
	for _, marker := range []string{"DROP_", "extra", "recovery", "expires_in", "concurrency", "rate_multiplier", "organization_id", "id_token"} {
		if strings.Contains(string(files[0].body), marker) {
			t.Errorf("forbidden field: %s", marker)
		}
	}
	minimal := `{"platform":"openai","type":"oauth","credentials":{"access_token":"dummy","account_id":"fallback","email":"fallback@example.test","id_token":"dummy-id","expires_in":3600},"plan_type":"pro"}`
	files, err = normalizeSub2API("fallback.json", []byte(syntheticSub2Export(minimal)))
	if err != nil {
		t.Fatal(err)
	}
	metadata = nil
	_ = json.Unmarshal(files[0].body, &metadata)
	if metadata["account_id"] != "fallback" || metadata["id_token"] != "dummy-id" || metadata["email"] != "fallback@example.test" || metadata["plan_type"] != "pro" || metadata["expired"] != "" {
		t.Fatal("fallback or optional mapping failed")
	}
	for _, name := range []string{"upload.JSON", strings.Repeat("界", 77) + ".json"} {
		first, errFirst := normalizeSub2API(name, []byte(syntheticSub2Export(minimal, minimal)))
		second, errSecond := normalizeSub2API(name, []byte(syntheticSub2Export(minimal, minimal)))
		if errFirst != nil || errSecond != nil || len(first) != 2 {
			t.Fatal("multi normalization failed")
		}
		for i := range first {
			if !safeAccountFileName(first[i].name) || first[i].name != second[i].name || strings.Contains(first[i].name, "fallback") {
				t.Fatal("unsafe or nondeterministic filename")
			}
		}
		if first[0].name == first[1].name {
			t.Fatal("duplicate generated names")
		}
	}
}

func TestNormalizeSub2APIRejectsMalformedBatch(t *testing.T) {
	invalid := []string{
		syntheticSub2Export(),
		strings.Replace(syntheticSub2Export(syntheticSub2Account), `"version":1`, `"version":2`, 1),
		syntheticSub2Export(syntheticSub2Account, `null`),
	}
	for _, replacement := range []struct{ old, new string }{
		{`"platform":"openai"`, `"platform":"claude"`},
		{`"type":"oauth"`, `"type":"apikey"`},
		{`"access_token":"dummy-access"`, `"access_token":" "`},
		{`"access_token":"dummy-access"`, `"access_token":null`},
		{`"expires_at":1700000000`, `"expires_at":"1700000000"`},
		{`"expires_at":1700000000`, `"expires_at":253402300800`},
		{`"expires_at":1700000000`, `"expires_at":1.5`},
		{`"email":"dummy@example.test"`, `"email":{}`},
		{`"openai_passthrough":true`, `"openai_passthrough":"true"`},
		{`"login_password":"DROP_PASSWORD"`, `"login_password":[]`},
	} {
		invalid = append(invalid, syntheticSub2Export(syntheticSub2Account, strings.Replace(syntheticSub2Account, replacement.old, replacement.new, 1)))
	}
	invalid = append(invalid, syntheticSub2Export(strings.Split(strings.Repeat(syntheticSub2Account+"|", maxAccountImportBatch+1), "|")[:maxAccountImportBatch+1]...))
	for i, body := range invalid {
		if files, err := normalizeSub2API("upload.json", []byte(body)); err == nil || files != nil {
			t.Errorf("malformed batch %d accepted", i)
		}
	}
}

func TestSub2APIImportBatch(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	store := sdkauth.NewFileTokenStore()
	store.SetBaseDir(dir)
	manager := coreauth.NewManager(store, nil, nil)
	api := &API{}
	cfg := &config.Config{AuthDir: dir}
	api.SetAccountManagement(management.NewHandler(cfg, "", manager))
	engine := gin.New()
	engine.POST("/import", api.importAccountFile)
	request := func(name, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/import?name="+name, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		engine.ServeHTTP(rec, req)
		for _, marker := range []string{"dummy-access", "DROP_", "DO_NOT_USE_FOR_PATH", "dummy-account"} {
			if strings.Contains(rec.Body.String(), marker) {
				t.Fatal("response leaked credential data")
			}
		}
		return rec
	}
	invalid := syntheticSub2Export(syntheticSub2Account, strings.Replace(syntheticSub2Account, `"platform":"openai"`, `"platform":"claude"`, 1))
	assertHTTPStatus(t, request("invalid.json", invalid), 422)
	entries, errRead := os.ReadDir(dir)
	if errRead != nil || len(entries) != 0 || len(manager.List()) != 0 {
		t.Fatal("invalid batch produced writes")
	}
	batch := syntheticSub2Export(syntheticSub2Account, syntheticSub2Account)
	rec := request("batch.json", batch)
	assertHTTPStatus(t, rec, 200)
	var result struct {
		Status   string   `json:"status"`
		Uploaded int      `json:"uploaded"`
		Files    []string `json:"files"`
		Failed   []struct {
			Name  string `json:"name"`
			Error string `json:"error"`
		} `json:"failed"`
	}
	if json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Uploaded != 2 || len(result.Files) != 2 || len(result.Failed) != 0 || result.Files[0] != "batch-001.json" || result.Files[1] != "batch-002.json" {
		t.Fatal("incorrect batch response")
	}
	persisted, errPersisted := os.ReadFile(filepath.Join(dir, "batch-001.json"))
	if errPersisted != nil || strings.Contains(string(persisted), "DROP_") {
		t.Fatal("unsafe persistence")
	}
	live, exists := manager.GetByID("batch-001.json")
	if !exists {
		t.Fatal("original uploader did not register auth")
	}
	upstream := httptest.NewRequest("POST", "https://example.test/responses", nil)
	if errPrepare := executor.NewCodexExecutor(&config.Config{}).PrepareRequest(upstream, live); errPrepare != nil {
		t.Fatal(errPrepare)
	}
	if upstream.Header.Get("Authorization") != "Bearer dummy-access" || live.Metadata["account_id"] != "dummy-account" {
		t.Fatal("normalized auth is not usable by original Codex executor")
	}
	rec = request("batch.json", batch)
	assertHTTPStatus(t, rec, 409)
	if json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Status != "error" || result.Uploaded != 0 || len(result.Failed) != 2 {
		t.Fatal("incorrect all-collision response")
	}
	after, errAfter := os.ReadFile(filepath.Join(dir, "batch-001.json"))
	if errAfter != nil || string(after) != string(persisted) {
		t.Fatal("duplicate import overwrote credential")
	}
	_, errRegister := manager.Register(context.Background(), &coreauth.Auth{ID: "partial-001.json", FileName: "partial-001.json", Provider: "claude", Disabled: true, Status: coreauth.StatusDisabled})
	if errRegister != nil {
		t.Fatal(errRegister)
	}
	rec = request("partial.json", batch)
	assertHTTPStatus(t, rec, 207)
	if json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Status != "partial" || result.Uploaded != 1 || len(result.Failed) != 1 || result.Failed[0].Name != "partial-001.json" || len(result.Files) != 1 || result.Files[0] != "partial-002.json" {
		t.Fatal("incorrect partial response")
	}
	cfg.AuthDir = filepath.Join(dir, "missing-parent")
	rec = request("io-failure.json", batch)
	assertHTTPStatus(t, rec, 500)
	if json.Unmarshal(rec.Body.Bytes(), &result) != nil || result.Status != "error" || result.Uploaded != 0 || len(result.Failed) != 2 {
		t.Fatal("incorrect IO failure response")
	}
}
