package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"unsafe"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"
)

func TestServerOptionsPreserveManagementAndPluginNoRoute(t *testing.T) {
	staticDir := t.TempDir()
	t.Setenv("MANAGEMENT_STATIC_PATH", staticDir)
	if errWrite := os.WriteFile(filepath.Join(staticDir, "management.html"), []byte("<html>management option test</html>"), 0o600); errWrite != nil {
		t.Fatalf("write management asset: %v", errWrite)
	}

	var calls []string
	host := pluginhost.New()
	server := newTestServerWithOptions(t,
		WithPluginHost(host),
		WithNoRouteHandler(func(c *gin.Context) bool {
			calls = append(calls, "carpool")
			if c.Request != nil && c.Request.URL != nil && strings.HasPrefix(c.Request.URL.Path, "/carpool/api/") {
				c.JSON(http.StatusNotFound, gin.H{"error": "carpool"})
				return true
			}
			return false
		}),
		WithNoRouteHandler(func(*gin.Context) bool {
			calls = append(calls, "next")
			return false
		}),
	)
	t.Cleanup(host.ShutdownAll)

	const pluginPath = "/v0/resource/plugins/carpool-option-test/status"
	installPluginResourceRoute(t, host, pluginPath, serverOptionManagementHandler(func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
		return pluginapi.ManagementResponse{
			StatusCode: http.StatusOK,
			Headers:    http.Header{"Content-Type": []string{"text/plain; charset=utf-8"}},
			Body:       []byte("plugin fallback"),
		}, nil
	}))

	management := httptest.NewRecorder()
	server.engine.ServeHTTP(management, httptest.NewRequest(http.MethodGet, "/management.html", nil))
	if management.Code != http.StatusOK || !strings.Contains(management.Body.String(), "management option test") {
		t.Fatalf("management response = %d %q", management.Code, management.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("registered management route reached NoRoute handlers: %#v", calls)
	}

	carpoolMissing := httptest.NewRecorder()
	server.engine.ServeHTTP(carpoolMissing, httptest.NewRequest(http.MethodGet, "/carpool/api/v1/missing", nil))
	if carpoolMissing.Code != http.StatusNotFound || carpoolMissing.Body.String() != "{\"error\":\"carpool\"}" {
		t.Fatalf("carpool NoRoute response = %d %q", carpoolMissing.Code, carpoolMissing.Body.String())
	}
	if !reflect.DeepEqual(calls, []string{"carpool"}) {
		t.Fatalf("carpool NoRoute calls = %#v, want first handler only", calls)
	}

	calls = nil
	pluginResponse := httptest.NewRecorder()
	server.engine.ServeHTTP(pluginResponse, httptest.NewRequest(http.MethodGet, pluginPath, nil))
	if pluginResponse.Code != http.StatusOK || pluginResponse.Body.String() != "plugin fallback" {
		t.Fatalf("plugin fallback response = %d %q", pluginResponse.Code, pluginResponse.Body.String())
	}
	if !reflect.DeepEqual(calls, []string{"carpool", "next"}) {
		t.Fatalf("plugin fallback NoRoute calls = %#v, want both custom handlers", calls)
	}
}

type serverOptionManagementHandler func(context.Context, pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error)

func (handler serverOptionManagementHandler) HandleManagement(ctx context.Context, request pluginapi.ManagementRequest) (pluginapi.ManagementResponse, error) {
	return handler(ctx, request)
}

func installPluginResourceRoute(t *testing.T, host *pluginhost.Host, routePath string, handler pluginapi.ManagementHandler) {
	t.Helper()
	const (
		pluginID      = "carpool-option-test"
		pluginFile    = "carpool-option-test.so"
		pluginVersion = "1.0.0"
	)

	hostValue := reflect.ValueOf(host).Elem()
	setPluginHostStringMap(t, hostValue.FieldByName("activePluginPaths"), pluginID, pluginFile)
	setPluginHostStringMap(t, hostValue.FieldByName("pluginFileVersions"), filepath.Clean(pluginFile), pluginVersion)
	setPluginHostStringMap(t, hostValue.FieldByName("activePluginVersions"), pluginID, pluginVersion)

	routes := writableServerOptionTestValue(hostValue.FieldByName("resourceRoutes"))
	record := reflect.New(routes.Type().Elem()).Elem()
	writableServerOptionTestValue(record.FieldByName("pluginID")).SetString(pluginID)
	writableServerOptionTestValue(record.FieldByName("path")).SetString(pluginFile)
	writableServerOptionTestValue(record.FieldByName("version")).SetString(pluginVersion)
	writableServerOptionTestValue(record.FieldByName("route")).Set(reflect.ValueOf(pluginapi.ResourceRoute{
		Path:    routePath,
		Handler: handler,
	}))
	routes.SetMapIndex(reflect.ValueOf(http.MethodGet+" "+routePath), record)
}

func setPluginHostStringMap(t *testing.T, field reflect.Value, key, value string) {
	t.Helper()
	field = writableServerOptionTestValue(field)
	if field.IsNil() {
		field.Set(reflect.MakeMap(field.Type()))
	}
	field.SetMapIndex(reflect.ValueOf(key), reflect.ValueOf(value))
}

func writableServerOptionTestValue(value reflect.Value) reflect.Value {
	return reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr())).Elem()
}
