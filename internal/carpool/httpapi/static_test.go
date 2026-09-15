package httpapi

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestRegisterStaticRoutes(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterStaticRoutes(engine)

	redirect := httptest.NewRecorder()
	engine.ServeHTTP(redirect, httptest.NewRequest(http.MethodGet, "/carpool", nil))
	if redirect.Code != http.StatusPermanentRedirect || redirect.Header().Get("Location") != "/" {
		t.Fatalf("/carpool response = %d, location %q", redirect.Code, redirect.Header().Get("Location"))
	}

	index := httptest.NewRecorder()
	engine.ServeHTTP(index, httptest.NewRequest(http.MethodGet, "/", nil))
	if index.Code != http.StatusOK || index.Header().Get("Content-Security-Policy") == "" || index.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("/ response = %d, headers %#v", index.Code, index.Header())
	}
	if index.Header().Get("Content-Type") != "text/html; charset=utf-8" {
		t.Fatalf("index content type = %q", index.Header().Get("Content-Type"))
	}

	asset := httptest.NewRecorder()
	engine.ServeHTTP(asset, httptest.NewRequest(http.MethodGet, "/assets/app.js", nil))
	if asset.Code != http.StatusOK || asset.Header().Get("ETag") == "" || asset.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("asset response = %d, headers %#v", asset.Code, asset.Header())
	}

	notModified := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/assets/app.js", nil)
	request.Header.Set("If-None-Match", asset.Header().Get("ETag"))
	engine.ServeHTTP(notModified, request)
	if notModified.Code != http.StatusNotModified || notModified.Body.Len() != 0 {
		t.Fatalf("conditional response = %d, body %q", notModified.Code, notModified.Body.String())
	}
}

func TestStaticRoutesDoNotCatchUnknownCarpoolPaths(t *testing.T) {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	RegisterStaticRoutes(engine)
	for _, path := range []string{"/carpool/missing", "/carpool/api/v1/missing", "/assets/missing.js"} {
		response := httptest.NewRecorder()
		engine.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusNotFound {
			t.Fatalf("GET %s status = %d, want 404", path, response.Code)
		}
	}
}
