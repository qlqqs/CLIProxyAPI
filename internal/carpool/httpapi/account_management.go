package httpapi

import (
	"bytes"
	"encoding/json"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/gin-gonic/gin"
	management "github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management"
)

const accountBodyLimit = 1 << 20

func safeAccountFileName(name string) bool {
	if !safeAccountTargetName(name) || len(name) > 240 || strings.HasPrefix(name, ".") || !strings.HasSuffix(strings.ToLower(name), ".json") || strings.ContainsAny(name, `<>:"|?*`) {
		return false
	}
	stem := strings.ToUpper(strings.SplitN(name, ".", 2)[0])
	switch stem {
	case "CON", "PRN", "AUX", "NUL":
		return false
	}
	if len(stem) == 4 && (strings.HasPrefix(stem, "COM") || strings.HasPrefix(stem, "LPT")) && stem[3] >= '1' && stem[3] <= '9' {
		return false
	}
	return true
}

func safeAccountTargetName(name string) bool {
	if name == "" || name != strings.TrimSpace(name) || len(name) > 256 || !utf8.ValidString(name) || strings.ContainsAny(name, `/\`) {
		return false
	}
	for _, r := range name {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

type ownedOAuth struct {
	session string
	expires time.Time
}
type accountManagement struct {
	handler *management.Handler
	mu      sync.Mutex
	states  map[string]ownedOAuth
}

// SetAccountManagement is called during server construction, before routes are served.
// Sharing this handler preserves token-store hooks and subsequent config reloads.
func (a *API) SetAccountManagement(h *management.Handler) { a.accounts.handler = h }

func (a *API) registerAccountManagement(admin *gin.RouterGroup) {
	group := admin.Group("", func(c *gin.Context) {
		if a.accounts.handler == nil {
			writeAPIError(c, 503, "accounts_unavailable", "账号管理暂不可用")
			c.Abort()
			return
		}
		c.Next()
	})
	group.GET("/auth-files", a.accountFiles)
	group.POST("/auth-files", a.requireMutation(), a.importAccountFile)
	group.PATCH("/auth-files/status", a.requireMutation(), a.accountStatus)
	group.POST("/codex-auth-url", a.requireMutation(), a.startAccountOAuth)
	group.GET("/get-auth-status", a.accountOAuthStatus)
	group.POST("/oauth-callback", a.requireMutation(), a.accountOAuthCallback)
}

// invokeAccountHandler never forwards raw management responses or error details.
func invokeAccountHandler(c *gin.Context, handler gin.HandlerFunc, method string, query url.Values, body []byte) (int, map[string]json.RawMessage) {
	request := c.Request.Clone(c.Request.Context())
	u := *request.URL
	u.RawQuery = query.Encode()
	request.URL = &u
	request.Method = method
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.Header = make(http.Header)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	delegated, _ := gin.CreateTestContext(recorder)
	delegated.Request = request
	handler(delegated)
	var result map[string]json.RawMessage
	_ = json.Unmarshal(recorder.Body.Bytes(), &result)
	return recorder.Code, result
}
func accountString(data map[string]json.RawMessage, key string) string {
	var value string
	_ = json.Unmarshal(data[key], &value)
	return value
}
func allowedAccountProvider(provider string) bool { return provider == "codex" || provider == "openai" }
func accountFailure(c *gin.Context, status int) {
	if status < 400 || status > 599 {
		status = 502
	}
	writeAPIError(c, status, "account_operation_failed", "账号操作失败")
}
func (a *API) accountEntries(c *gin.Context) ([]map[string]json.RawMessage, bool) {
	status, result := invokeAccountHandler(c, a.accounts.handler.ListAuthFiles, "GET", nil, nil)
	if status != 200 {
		accountFailure(c, status)
		return nil, false
	}
	var entries []map[string]json.RawMessage
	if json.Unmarshal(result["files"], &entries) != nil {
		accountFailure(c, 502)
		return nil, false
	}
	for _, hidden := range a.accounts.handler.CarpoolHiddenAccountEntries() {
		encoded, _ := json.Marshal(hidden)
		var entry map[string]json.RawMessage
		_ = json.Unmarshal(encoded, &entry)
		entries = append(entries, entry)
	}
	return entries, true
}
func (a *API) accountFiles(c *gin.Context) {
	entries, ok := a.accountEntries(c)
	if !ok {
		return
	}
	files := make([]gin.H, 0)
	for _, entry := range entries {
		provider := accountString(entry, "provider")
		name := accountString(entry, "name")
		if !allowedAccountProvider(provider) || !safeAccountTargetName(name) {
			continue
		}
		var disabled bool
		_ = json.Unmarshal(entry["disabled"], &disabled)
		status := accountString(entry, "status")
		switch status {
		case "active", "disabled", "error", "pending":
		default:
			status = "unknown"
		}
		file := gin.H{"name": name, "auth_index": accountString(entry, "auth_index"), "provider": provider, "type": provider, "disabled": disabled, "status": status}
		// Labels and email are display fields; never expose AccountInfo's raw account value.
		for _, key := range []string{"label", "email"} {
			if value := accountString(entry, key); len(value) <= 256 {
				file[key] = value
			}
		}
		for _, key := range []string{"created_at", "updated_at", "last_refresh"} {
			if value := accountString(entry, key); value != "" {
				if _, errParse := time.Parse(time.RFC3339Nano, value); errParse == nil {
					file[key] = value
				}
			}
		}
		var claims map[string]json.RawMessage
		_ = json.Unmarshal(entry["id_token"], &claims)
		switch plan := accountString(claims, "plan_type"); plan {
		case "free", "plus", "pro", "team", "business", "enterprise", "edu":
			file["plan_type"] = plan
		}
		files = append(files, file)
	}
	c.JSON(200, gin.H{"files": files})
}
func (a *API) accountNameAllowed(c *gin.Context, name, index string, mustExist bool) bool {
	if (!mustExist && !safeAccountFileName(name)) || (mustExist && !safeAccountTargetName(name)) {
		accountFailure(c, 422)
		return false
	}
	status := a.accounts.handler.ValidateCarpoolAccountTarget(name, index, !mustExist)
	if status != 200 {
		if !mustExist && status == 409 {
			writeAPIError(c, 409, "account_file_exists", "同名账号文件已存在，请重命名文件后重试；不会覆盖现有账号")
		} else {
			accountFailure(c, status)
		}
		return false
	}
	return true
}
func readAccountBody(c *gin.Context) ([]byte, bool) {
	body, err := io.ReadAll(io.LimitReader(c.Request.Body, accountBodyLimit+1))
	if len(body) > accountBodyLimit {
		accountFailure(c, 413)
		return nil, false
	}
	if err != nil {
		accountFailure(c, 400)
		return nil, false
	}
	return body, true
}
func (a *API) importAccountFile(c *gin.Context) {
	body, ok := readAccountBody(c)
	if !ok {
		return
	}
	name := c.Query("name")
	contentType, params, err := mime.ParseMediaType(c.GetHeader("Content-Type"))
	if err != nil {
		accountFailure(c, 415)
		return
	}
	if contentType == "multipart/form-data" {
		reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
		part, errPart := reader.NextPart()
		if errPart != nil {
			accountFailure(c, 422)
			return
		}
		_, disposition, errDisposition := mime.ParseMediaType(part.Header.Get("Content-Disposition"))
		if errDisposition != nil || (part.FormName() != "file" && part.FormName() != "files") {
			accountFailure(c, 422)
			return
		}
		name = disposition["filename"]
		body, err = io.ReadAll(part)
		if err != nil {
			accountFailure(c, 422)
			return
		}
		if _, errNext := reader.NextPart(); errNext != io.EOF {
			accountFailure(c, 422)
			return
		}
	} else if contentType != "application/json" {
		accountFailure(c, 415)
		return
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(body, &metadata) == nil && accountString(metadata, "type") == "sub2api-data" {
		files, errNormalize := normalizeSub2API(name, body)
		if errNormalize != nil {
			accountFailure(c, 422)
			return
		}
		a.importNormalizedAccounts(c, files)
		return
	}
	if json.Unmarshal(body, &metadata) != nil || metadata == nil || !allowedAccountProvider(accountString(metadata, "type")) {
		accountFailure(c, 422)
		return
	}
	// Require usable credentials, not arbitrary JSON carrying a provider label.
	if accountString(metadata, "type") != "codex" || strings.TrimSpace(accountString(metadata, "access_token")) == "" {
		writeAPIError(c, 422, "unsupported_credential_shape", "JSON 导入仅支持含 access_token 的 Codex 授权文件；API Key 请使用原版管理配置")
		return
	}
	if !a.accountNameAllowed(c, name, "", false) {
		return
	}
	c.Request = c.Request.WithContext(management.WithCreateOnlyAuthFile(c.Request.Context()))
	status, _ := invokeAccountHandler(c, a.accounts.handler.UploadAuthFile, "POST", url.Values{"name": {name}}, body)
	if status == 409 {
		writeAPIError(c, 409, "account_file_exists", "同名账号文件已存在，请重命名文件后重试；不会覆盖现有账号")
		return
	}
	if status != 200 {
		accountFailure(c, status)
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}

// importNormalizedAccounts preflights every filename across all providers before
// writing. The shared create-only upload context closes races after preflight.
func (a *API) importNormalizedAccounts(c *gin.Context, files []normalizedAccountImport) {
	statuses := make([]int, len(files))
	for i, file := range files {
		statuses[i] = a.accounts.handler.ValidateCarpoolAccountTarget(file.name, "", true)
	}
	uploaded := make([]string, 0, len(files))
	failed := make([]gin.H, 0)
	conflicts := 0
	c.Request = c.Request.WithContext(management.WithCreateOnlyAuthFile(c.Request.Context()))
	for i, file := range files {
		status := statuses[i]
		if status == 200 {
			status, _ = invokeAccountHandler(c, a.accounts.handler.UploadAuthFile, "POST", url.Values{"name": {file.name}}, file.body)
		}
		if status == 200 {
			uploaded = append(uploaded, file.name)
			continue
		}
		message := "账号文件导入失败"
		if status == 409 {
			conflicts++
			message = "同名账号文件已存在，不会覆盖现有账号"
		}
		failed = append(failed, gin.H{"name": file.name, "error": message})
	}
	status, result := 200, "ok"
	if len(failed) > 0 {
		status, result = 207, "partial"
		if len(uploaded) == 0 {
			status, result = 500, "error"
			if conflicts == len(files) {
				status = 409
			}
		}
	}
	c.JSON(status, gin.H{"status": result, "uploaded": len(uploaded), "files": uploaded, "failed": failed})
}

func (a *API) accountStatus(c *gin.Context) {
	var request struct {
		Name      string `json:"name"`
		AuthIndex string `json:"auth_index,omitempty"`
		Disabled  *bool  `json:"disabled"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	if request.Disabled == nil {
		accountFailure(c, 422)
		return
	}
	if !a.accountNameAllowed(c, request.Name, request.AuthIndex, true) {
		return
	}
	body, _ := json.Marshal(request)
	c.Request = c.Request.WithContext(management.WithPersistentAuthStatus(c.Request.Context()))
	status, _ := invokeAccountHandler(c, a.accounts.handler.PatchAuthFileStatus, "PATCH", nil, body)
	if status != 200 {
		accountFailure(c, status)
		return
	}
	c.JSON(200, gin.H{"status": "ok", "disabled": *request.Disabled})
}
func (a *API) startAccountOAuth(c *gin.Context) {
	if _, ok := readAccountBody(c); !ok {
		return
	}
	identity, _ := currentIdentity(c)
	a.accounts.mu.Lock()
	defer a.accounts.mu.Unlock()
	if a.accounts.states == nil {
		a.accounts.states = make(map[string]ownedOAuth)
	}
	for state, owner := range a.accounts.states {
		if !a.now().Before(owner.expires) {
			delete(a.accounts.states, state)
		}
	}
	if len(a.accounts.states) >= 256 {
		accountFailure(c, 429)
		return
	}
	// Manual callback submission avoids opening a shared loopback forwarder.
	status, result := invokeAccountHandler(c, a.accounts.handler.RequestCodexToken, "GET", nil, nil)
	if status != 200 {
		accountFailure(c, status)
		return
	}
	state := accountString(result, "state")
	authURL := accountString(result, "url")
	if state == "" || authURL == "" {
		accountFailure(c, 502)
		return
	}
	a.accounts.states[state] = ownedOAuth{session: identity.Session.ID, expires: a.now().Add(10 * time.Minute)}
	c.JSON(200, gin.H{"status": "ok", "state": state, "url": authURL})
}
func (a *API) ownsAccountOAuth(c *gin.Context, state string) bool {
	identity, _ := currentIdentity(c)
	a.accounts.mu.Lock()
	owner, ok := a.accounts.states[state]
	a.accounts.mu.Unlock()
	if !ok || owner.session != identity.Session.ID || !a.now().Before(owner.expires) {
		accountFailure(c, 404)
		return false
	}
	provider, _, isPlugin, _, _, exists := management.GetOAuthSessionDetails(state)
	if !exists || provider != "codex" || isPlugin {
		accountFailure(c, 404)
		return false
	}
	return true
}
func (a *API) accountOAuthStatus(c *gin.Context) {
	state := c.Query("state")
	if !a.ownsAccountOAuth(c, state) {
		return
	}
	status, result := invokeAccountHandler(c, a.accounts.handler.GetAuthStatus, "GET", url.Values{"state": {state}}, nil)
	if status != 200 {
		accountFailure(c, status)
		return
	}
	value := accountString(result, "status")
	switch value {
	case "ok", "wait":
		c.JSON(200, gin.H{"status": value})
	default:
		c.JSON(200, gin.H{"status": "error", "error": "OAuth 授权失败或已过期"})
	}
}
func (a *API) accountOAuthCallback(c *gin.Context) {
	var request struct {
		Provider    string `json:"provider"`
		State       string `json:"state"`
		Code        string `json:"code"`
		Error       string `json:"error"`
		RedirectURL string `json:"redirect_url"`
	}
	if !decodeJSON(c, &request) {
		return
	}
	if request.Provider != "" && request.Provider != "codex" {
		accountFailure(c, 422)
		return
	}
	if request.RedirectURL != "" {
		parsed, err := url.Parse(request.RedirectURL)
		if err != nil {
			accountFailure(c, 422)
			return
		}
		query := parsed.Query()
		if request.State != "" && query.Get("state") != "" && request.State != query.Get("state") {
			accountFailure(c, 422)
			return
		}
		if request.State == "" {
			request.State = query.Get("state")
		}
		if request.Code == "" {
			request.Code = query.Get("code")
		}
		if request.Error == "" {
			request.Error = query.Get("error")
		}
	}
	if !a.ownsAccountOAuth(c, request.State) {
		return
	}
	body, _ := json.Marshal(gin.H{"provider": "codex", "state": request.State, "code": request.Code, "error": request.Error})
	status, _ := invokeAccountHandler(c, a.accounts.handler.PostOAuthCallback, "POST", nil, body)
	if status != 200 {
		accountFailure(c, status)
		return
	}
	c.JSON(200, gin.H{"status": "ok"})
}
