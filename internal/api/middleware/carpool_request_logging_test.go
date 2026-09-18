package middleware

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

func TestCarpoolControlAPIsNeverCaptureCredentials(t *testing.T) {
	gin.SetMode(gin.TestMode)
	for _, enabled := range []bool{false, true} {
		for _, status := range []int{http.StatusOK, http.StatusBadRequest, http.StatusInternalServerError} {
			t.Run(fmt.Sprintf("enabled=%t/status=%d", enabled, status), func(t *testing.T) {
				logsDir := t.TempDir()
				logger := logging.NewFileRequestLogger(enabled, logsDir, "", 10)
				engine := gin.New()
				engine.Use(RequestLoggingMiddleware(logger))
				paths := []string{"/carpool/api/v1/session", "/carpool/api/v1/admin/auth-files", "/carpool/api/v1/admin/auth-files/status", "/carpool/api/v1/admin/codex-auth-url", "/carpool/api/v1/admin/oauth-callback"}
				const payload = `{"access_token":"synthetic-upload-secret","password":"synthetic-password"}`
				for _, path := range paths {
					engine.POST(path, func(c *gin.Context) {
						body, errRead := io.ReadAll(c.Request.Body)
						if errRead != nil || string(body) != payload {
							t.Errorf("request body altered: %v", errRead)
						}
						c.JSON(status, gin.H{"secret": "synthetic-response-secret"})
					})
					request := httptest.NewRequest(http.MethodPost, path, strings.NewReader(payload))
					request.Header.Set("Content-Type", "application/json")
					request.Header.Set("Cookie", "cpa_carpool_session=synthetic-session-secret")
					response := httptest.NewRecorder()
					engine.ServeHTTP(response, request)
					if response.Code != status {
						t.Fatalf("status = %d, want %d", response.Code, status)
					}
				}
				entries, errRead := os.ReadDir(logsDir)
				if errRead != nil {
					t.Fatal(errRead)
				}
				if len(entries) != 0 {
					t.Fatalf("control API created request logs: %v", entries)
				}
			})
		}
	}
}

func TestCarpoolLoggingExclusionDoesNotHideProxyRequests(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/v1/chat/completions", "/backend-api/codex/responses", "/carpool/api-extra"} {
		if !shouldLogRequest(path) {
			t.Errorf("proxy/unrelated path %q must remain loggable", path)
		}
	}
	for _, path := range []string{"/carpool/api", "/carpool/api/v1/admin/auth-files", "/v0/management/auth-files"} {
		if shouldLogRequest(path) {
			t.Errorf("credential control path %q must not be logged", path)
		}
	}
}
