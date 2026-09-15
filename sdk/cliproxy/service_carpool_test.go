package cliproxy

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	carpoolaccess "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/access"
	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/pluginhost"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v7/sdk/access"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginabi"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestCommitConfigUpdateWarnsWhenCarpoolSettingsRequireRestart(t *testing.T) {
	initial := &internalconfig.Config{Carpool: internalconfig.DefaultCarpoolConfig()}
	initial.Carpool.Enabled = true
	service := &Service{cfg: initial}
	hook := logtest.NewLocal(log.StandardLogger())
	t.Cleanup(hook.Reset)

	changed := *initial
	changed.Carpool.DatabasePath = "./data/reloaded-carpool.db"
	service.commitConfigUpdate(&changed)

	const warning = "carpool configuration changed; restart CLIProxyAPI to apply the new carpool settings"
	entries := hook.AllEntries()
	if len(entries) != 1 || entries[0].Level != log.WarnLevel || entries[0].Message != warning {
		t.Fatalf("carpool restart warnings = %#v, want one warning %q", entries, warning)
	}
	if len(entries[0].Data) != 0 {
		t.Fatalf("carpool restart warning contains fields: %#v", entries[0].Data)
	}

	hook.Reset()
	unrelated := changed
	unrelated.Debug = true
	service.commitConfigUpdate(&unrelated)
	if entries = hook.AllEntries(); len(entries) != 0 {
		t.Fatalf("unrelated config change emitted carpool warning: %#v", entries)
	}
}

func TestCarpoolConfigChangeRequiresRestart(t *testing.T) {
	disabled := &internalconfig.Config{Carpool: internalconfig.DefaultCarpoolConfig()}
	disabled.Carpool.Enabled = false
	enabled := &internalconfig.Config{Carpool: internalconfig.DefaultCarpoolConfig()}
	changedSession := *enabled
	changedSession.Carpool.Session.IdleTTL = "1h"

	tests := []struct {
		name string
		old  *internalconfig.Config
		new  *internalconfig.Config
		want bool
	}{
		{name: "nil old", old: nil, new: enabled, want: false},
		{name: "both disabled", old: disabled, new: &internalconfig.Config{}, want: false},
		{name: "enable", old: disabled, new: enabled, want: true},
		{name: "disable", old: enabled, new: disabled, want: true},
		{name: "session change", old: enabled, new: &changedSession, want: true},
		{name: "unchanged", old: enabled, new: enabled, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := carpoolConfigChangeRequiresRestart(test.old, test.new); got != test.want {
				t.Fatalf("carpoolConfigChangeRequiresRestart() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestBuilderBuildLeavesCarpoolDisabledWithoutSideEffects(t *testing.T) {
	root, databasePath, configPath := carpoolBuilderTestPaths(t)
	cfg := carpoolBuilderTestConfig(root, databasePath, false)
	accessManager := sdkaccess.NewManager()

	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithRequestAccessManager(accessManager).
		WithCoreAuthManager(coreauth.NewManager(nil, nil, nil)).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}
	if service.carpoolModule != nil {
		t.Fatal("Build() initialized carpool while it was disabled")
	}
	if len(service.serverOptions) != 3 {
		t.Fatalf("disabled server options len = %d, want 3 host options only", len(service.serverOptions))
	}
	for _, provider := range accessManager.Providers() {
		if provider != nil && provider.Identifier() == carpoolaccess.ProviderName {
			t.Fatal("disabled Build() registered the carpool access provider")
		}
	}
	if _, errStat := os.Stat(filepath.Dir(databasePath)); !os.IsNotExist(errStat) {
		t.Fatalf("disabled Build() created database directory, stat error = %v", errStat)
	}
	if service.pluginHost != nil {
		t.Cleanup(service.pluginHost.ShutdownAll)
	}
}

func TestBuilderBuildInitializesCarpoolModuleAndProvider(t *testing.T) {
	root, databasePath, configPath := carpoolBuilderTestPaths(t)
	cfg := carpoolBuilderTestConfig(root, databasePath, true)
	accessManager := sdkaccess.NewManager()

	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithRequestAccessManager(accessManager).
		WithCoreAuthManager(coreauth.NewManager(nil, nil, nil)).
		Build()
	if errBuild != nil {
		t.Fatalf("Build() error = %v", errBuild)
	}
	if service.carpoolModule == nil {
		t.Fatal("Build() did not initialize enabled carpool module")
	}
	t.Cleanup(func() {
		_ = service.carpoolModule.Close(context.Background())
		if service.pluginHost != nil {
			service.pluginHost.ShutdownAll()
		}
	})

	foundCarpoolProvider := false
	for _, provider := range accessManager.Providers() {
		if provider != nil && provider.Identifier() == carpoolaccess.ProviderName {
			foundCarpoolProvider = true
			break
		}
	}
	if !foundCarpoolProvider {
		t.Fatalf("request providers = %#v, want %q", accessManager.Providers(), carpoolaccess.ProviderName)
	}
	if _, errStat := os.Stat(databasePath); errStat != nil {
		t.Fatalf("enabled Build() database stat error = %v", errStat)
	}
}

func TestBuilderBuildRejectsCarpoolWithExclusiveFrontendAuthProvider(t *testing.T) {
	root, databasePath, configPath := carpoolBuilderTestPaths(t)
	const pluginID = "exclusive-carpool-test"
	pluginsDir := filepath.Join(root, "plugins")
	platformDir := filepath.Join(pluginsDir, runtime.GOOS, runtime.GOARCH)
	if errMkdir := os.MkdirAll(platformDir, 0o700); errMkdir != nil {
		t.Fatalf("create plugin directory: %v", errMkdir)
	}
	pluginPath := filepath.Join(platformDir, pluginID+pluginhost.PluginExtension(runtime.GOOS))
	if errWrite := os.WriteFile(pluginPath, []byte("test plugin placeholder"), 0o600); errWrite != nil {
		t.Fatalf("write plugin placeholder: %v", errWrite)
	}

	host := pluginhost.New()
	seedLoadedPlugin(t, host, pluginID, pluginPath, &exclusiveFrontendAuthPluginClient{})
	previousExclusive := sdkaccess.ExclusiveProvider()
	t.Cleanup(func() {
		host.ShutdownAll()
		sdkaccess.UnregisterProvider("plugin:" + pluginID + ":exclusive-test")
		if previousExclusive == "" {
			sdkaccess.ClearExclusiveProvider()
		} else {
			sdkaccess.SetExclusiveProvider(previousExclusive)
		}
	})

	enabled := true
	cfg := carpoolBuilderTestConfig(root, databasePath, true)
	cfg.Plugins = internalconfig.PluginsConfig{
		Enabled: true,
		Dir:     pluginsDir,
		Configs: map[string]internalconfig.PluginInstanceConfig{
			pluginID: {Enabled: &enabled},
		},
	}

	service, errBuild := NewBuilder().
		WithConfig(cfg).
		WithConfigPath(configPath).
		WithPluginHost(host).
		WithCoreAuthManager(coreauth.NewManager(nil, nil, nil)).
		Build()
	if errBuild == nil {
		t.Fatal("Build() accepted carpool with an exclusive frontend authentication plugin")
	}
	if service != nil {
		t.Fatal("Build() returned a service for an exclusive frontend authentication conflict")
	}
	want := "cliproxy: carpool cannot be enabled with an exclusive frontend authentication plugin"
	if errBuild.Error() != want {
		t.Fatalf("Build() error = %q, want %q", errBuild, want)
	}
	if _, errStat := os.Stat(filepath.Dir(databasePath)); !os.IsNotExist(errStat) {
		t.Fatalf("conflicting Build() opened the carpool database, stat error = %v", errStat)
	}
}

type exclusiveFrontendAuthPluginClient struct{}

func (*exclusiveFrontendAuthPluginClient) Call(_ context.Context, method string, _ []byte) ([]byte, error) {
	switch method {
	case pluginabi.MethodPluginRegister, pluginabi.MethodPluginReconfigure:
		return carpoolPluginResult(map[string]any{
			"schema_version": pluginabi.SchemaVersion,
			"metadata": map[string]any{
				"Name":             "exclusive carpool test",
				"Version":          "1.0.0",
				"Author":           "test",
				"GitHubRepository": "https://github.com/router-for-me/CLIProxyAPI",
			},
			"capabilities": map[string]any{
				"frontend_auth_provider":           true,
				"frontend_auth_provider_exclusive": true,
			},
		})
	case pluginabi.MethodFrontendAuthIdentifier:
		return carpoolPluginResult(map[string]string{"identifier": "exclusive-test"})
	case pluginabi.MethodPluginQuiesce, pluginabi.MethodPluginShutdown:
		return carpoolPluginResult(struct{}{})
	default:
		return nil, fmt.Errorf("unexpected plugin method %q", method)
	}
}

func (*exclusiveFrontendAuthPluginClient) Shutdown() {}

func carpoolPluginResult(value any) ([]byte, error) {
	result, errMarshal := json.Marshal(value)
	if errMarshal != nil {
		return nil, errMarshal
	}
	return json.Marshal(pluginabi.Envelope{OK: true, Result: result})
}

func seedLoadedPlugin(t *testing.T, host *pluginhost.Host, pluginID, pluginPath string, client any) {
	t.Helper()
	hostValue := reflect.ValueOf(host).Elem()
	loaded := writableCarpoolTestValue(hostValue.FieldByName("loaded"))
	loadedPlugin := reflect.New(loaded.Type().Elem().Elem())
	loadedPluginValue := loadedPlugin.Elem()
	writableCarpoolTestValue(loadedPluginValue.FieldByName("id")).SetString(pluginID)
	writableCarpoolTestValue(loadedPluginValue.FieldByName("path")).SetString(filepath.Clean(pluginPath))
	writableCarpoolTestValue(loadedPluginValue.FieldByName("client")).Set(reflect.ValueOf(client))
	loaded.SetMapIndex(reflect.ValueOf(pluginID), loadedPlugin)
}

func writableCarpoolTestValue(value reflect.Value) reflect.Value {
	return reflect.NewAt(value.Type(), unsafe.Pointer(value.UnsafeAddr())).Elem()
}

func carpoolBuilderTestConfig(root, databasePath string, enabled bool) *internalconfig.Config {
	carpoolConfig := internalconfig.DefaultCarpoolConfig()
	carpoolConfig.Enabled = enabled
	carpoolConfig.DatabasePath = databasePath
	return &internalconfig.Config{
		AuthDir: filepath.Join(root, "auth"),
		Carpool: carpoolConfig,
	}
}

func carpoolBuilderTestPaths(t *testing.T) (root, databasePath, configPath string) {
	t.Helper()
	root = t.TempDir()
	reportedTemp := filepath.Join(root, "reported-temp")
	t.Setenv("TMPDIR", reportedTemp)
	t.Setenv("TMP", reportedTemp)
	t.Setenv("TEMP", reportedTemp)
	sdkaccess.ClearExclusiveProvider()
	return root, filepath.Join(root, "database", "carpool.db"), filepath.Join(root, "config.yaml")
}

func carpoolProviderIdentifiers(providers []sdkaccess.Provider) string {
	identifiers := make([]string, 0, len(providers))
	for _, provider := range providers {
		if provider != nil {
			identifiers = append(identifiers, provider.Identifier())
		}
	}
	return strings.Join(identifiers, ",")
}
