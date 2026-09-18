package management

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCreateOnlyUploadConcurrentWinnerMatchesRuntime(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	manager := coreauth.NewManager(nil, nil, nil)
	handler := &Handler{cfg: &config.Config{AuthDir: dir}, authManager: manager}
	start := make(chan struct{})
	results := make(chan int, 2)
	for _, token := range []string{"first-token", "second-token"} {
		go func(token string) {
			request := httptest.NewRequest("POST", "/auth-files?name=concurrent.json", strings.NewReader(`{"type":"codex","access_token":"`+token+`"}`))
			request.Header.Set("Content-Type", "application/json")
			request = request.WithContext(WithCreateOnlyAuthFile(context.Background()))
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = request
			<-start
			handler.UploadAuthFile(ctx)
			results <- rec.Code
		}(token)
	}
	close(start)
	codes := map[int]int{(<-results): 1}
	codes[<-results]++
	if codes[200] != 1 || codes[409] != 1 {
		t.Fatalf("concurrent upload statuses: %#v", codes)
	}
	raw, errRead := os.ReadFile(filepath.Join(dir, "concurrent.json"))
	if errRead != nil {
		t.Fatal(errRead)
	}
	var disk map[string]any
	if errDecode := json.Unmarshal(raw, &disk); errDecode != nil {
		t.Fatal(errDecode)
	}
	live, ok := manager.GetByID("concurrent.json")
	if !ok || live.Metadata["access_token"] != disk["access_token"] {
		t.Fatal("runtime and persisted winner differ")
	}
	// The opt-in must not change legacy overwrite semantics.
	rec := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(rec)
	ctx.Request = httptest.NewRequest("POST", "/auth-files?name=concurrent.json", strings.NewReader(`{"type":"codex","access_token":"legacy-replacement"}`))
	ctx.Request.Header.Set("Content-Type", "application/json")
	handler.UploadAuthFile(ctx)
	if rec.Code != 200 {
		t.Fatalf("legacy replacement failed: %d", rec.Code)
	}
	live, _ = manager.GetByID("concurrent.json")
	if live.Metadata["access_token"] != "legacy-replacement" {
		t.Fatal("legacy overwrite changed")
	}
}
