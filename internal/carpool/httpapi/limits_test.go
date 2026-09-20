package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
)

func TestProxyGuardReleasesOnEarlyFailureAndStreamEnd(t *testing.T) {
	f := newProxyAuthorizationFixture(t)
	ctx := t.Context()
	one := 1
	if errSet := f.store.SetAccountConcurrency(ctx, f.authID, &one, nil); errSet != nil {
		t.Fatal(errSet)
	}
	engine := gin.New()
	engine.Use(f.api.ProxyCredentialGuard())
	engine.POST("/v1/chat/completions", func(c *gin.Context) {
		_, errAuth := f.accessManager.Authenticate(c.Request.Context(), c.Request)
		if errAuth != nil {
			c.AbortWithStatusJSON(errAuth.HTTPStatusCode(), gin.H{"error": errAuth.Message})
			return
		}
		if c.GetHeader("X-Test-Early") == "true" {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid body"})
			return
		}
		c.Header("Content-Type", "text/event-stream")
		c.String(http.StatusOK, "data: done\n\n")
	})
	for index := 0; index < 4; index++ {
		// A guard deadline makes leaked ownership fail the test rather than hang it.
		requestCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+f.model+`"}`)).WithContext(requestCtx)
		request.Header.Set("Authorization", "Bearer "+f.token)
		if index%2 == 0 {
			request.Header.Set("X-Test-Early", "true")
		}
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, request)
		cancel()
		want := http.StatusOK
		if index%2 == 0 {
			want = http.StatusBadRequest
		}
		if response.Code != want {
			t.Fatalf("request %d: %d %s", index, response.Code, response.Body.String())
		}
	}
}

func TestMemberLimitsRequestNullAndOmitted(t *testing.T) {
	var request memberLimitsRequest
	if err := json.Unmarshal([]byte(`{"five_hour_limit_usd":"0","weekly_limit_usd":null}`), &request); err != nil {
		t.Fatal(err)
	}
	if _, errUpdate := request.update(); !errors.Is(errUpdate, domain.ErrInvalid) {
		t.Fatalf("null quota update error = %v", errUpdate)
	}
}
