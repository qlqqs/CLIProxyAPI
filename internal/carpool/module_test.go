package carpool

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

type moduleNoopUsagePlugin struct{}

func (moduleNoopUsagePlugin) HandleUsage(context.Context, usage.Record) {}

func TestOpenDisabledDoesNotCreateDatabaseOrIntegrations(t *testing.T) {
	root, databasePath, configPath := moduleTestPaths(t)
	cfg := moduleTestConfig(databasePath, false)

	module, errOpen := Open(context.Background(), cfg, configPath, coreauth.NewManager(nil, nil, nil))
	if errOpen == nil || !strings.Contains(errOpen.Error(), "module is disabled") {
		t.Fatalf("Open() error = %v, want disabled error", errOpen)
	}
	if module != nil {
		t.Fatal("Open() returned a module while carpool is disabled")
	}
	if _, errStat := os.Stat(filepath.Join(root, "database")); !os.IsNotExist(errStat) {
		t.Fatalf("disabled Open() created database directory, stat error = %v", errStat)
	}
	var nilModule *Module
	if nilModule.Provider() != nil || nilModule.Control() != nil || len(nilModule.ServerOptions()) != 0 {
		t.Fatal("disabled module exposed a provider, control, or server options")
	}
}

func TestModuleStartsAndClosesIdempotently(t *testing.T) {
	_, databasePath, configPath := moduleTestPaths(t)
	module, errOpen := Open(
		context.Background(),
		moduleTestConfig(databasePath, true),
		configPath,
		coreauth.NewManager(nil, nil, nil),
	)
	if errOpen != nil {
		t.Fatalf("Open() error = %v", errOpen)
	}
	t.Cleanup(func() {
		_ = module.Close(context.Background())
		usage.RegisterNamedPlugin(accountingPluginName, moduleNoopUsagePlugin{})
	})

	if module.Provider() == nil || module.Provider().Identifier() != "carpool-user-key" {
		t.Fatalf("Provider() = %#v, want carpool-user-key provider", module.Provider())
	}
	if module.Control() == nil {
		t.Fatal("Control() = nil")
	}
	if options := module.ServerOptions(); len(options) != 5 {
		t.Fatalf("ServerOptions() len = %d, want 5", len(options))
	}
	if _, errStat := os.Stat(databasePath); errStat != nil {
		t.Fatalf("database stat error = %v", errStat)
	}

	module.Start()
	module.Start()
	if errClose := module.Close(context.Background()); errClose != nil {
		t.Fatalf("first Close() error = %v", errClose)
	}
	if errClose := module.Close(context.Background()); errClose != nil {
		t.Fatalf("second Close() error = %v", errClose)
	}
	if _, errVersion := module.store.SchemaVersion(context.Background()); errVersion == nil {
		t.Fatal("store remained usable after Close()")
	}

	before := module.writer.Snapshot().DroppedUsage
	module.writer.HandleUsage(context.Background(), usage.Record{RequestID: "request-after-close", AuthID: "auth-after-close"})
	if after := module.writer.Snapshot().DroppedUsage; after != before+1 {
		t.Fatalf("DroppedUsage after Close() = %d, want %d", after, before+1)
	}
}

func TestOpenWarnsWhenSecureSessionCookieIsDisabled(t *testing.T) {
	_, databasePath, configPath := moduleTestPaths(t)
	cfg := moduleTestConfig(databasePath, true)
	cfg.Carpool.Session.CookieSecure = false
	hook := &moduleLogHook{}
	logger := log.StandardLogger()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	logger.AddHook(hook)
	t.Cleanup(func() { logger.ReplaceHooks(oldHooks) })

	module, errOpen := Open(context.Background(), cfg, configPath, coreauth.NewManager(nil, nil, nil))
	if errOpen != nil {
		t.Fatalf("Open() error = %v", errOpen)
	}
	t.Cleanup(func() { _ = module.Close(context.Background()) })

	const warning = "carpool session cookie Secure attribute is disabled; use only for local HTTP development"
	for _, entry := range hook.entries {
		if entry.Level == log.WarnLevel && entry.Message == warning {
			if len(entry.Data) != 0 {
				t.Fatalf("cookie security warning contains fields: %#v", entry.Data)
			}
			return
		}
	}
	t.Fatalf("cookie security warning was not logged: %#v", hook.entries)
}

func TestOpenClosesDatabaseAfterRecoveryInitializationFailure(t *testing.T) {
	root, databasePath, configPath := moduleTestPaths(t)
	ctx := context.Background()

	seed, errOpen := carpoolsqlite.Open(ctx, carpoolsqlite.Config{Path: databasePath})
	if errOpen != nil {
		t.Fatalf("seed sqlite Open() error = %v", errOpen)
	}
	if errClose := seed.Close(); errClose != nil {
		t.Fatalf("seed sqlite Close() error = %v", errClose)
	}

	rawDB, errRawOpen := sql.Open("sqlite", "file:"+filepath.ToSlash(databasePath))
	if errRawOpen != nil {
		t.Fatalf("sql.Open() error = %v", errRawOpen)
	}
	if _, errDrop := rawDB.ExecContext(ctx, "DROP TABLE proxy_requests"); errDrop != nil {
		_ = rawDB.Close()
		t.Fatalf("drop proxy_requests error = %v", errDrop)
	}
	if errClose := rawDB.Close(); errClose != nil {
		t.Fatalf("raw database Close() error = %v", errClose)
	}

	module, errModuleOpen := Open(ctx, moduleTestConfig(databasePath, true), configPath, coreauth.NewManager(nil, nil, nil))
	if errModuleOpen == nil || !strings.Contains(errModuleOpen.Error(), "recover interrupted requests") {
		t.Fatalf("Open() error = %v, want recovery initialization error", errModuleOpen)
	}
	if module != nil {
		t.Fatal("Open() returned a module after recovery initialization failed")
	}
	assertNoOpenDatabaseDescriptors(t, databasePath)

	databaseDir := filepath.Dir(databasePath)
	failedDir := filepath.Join(root, "failed-database")
	if errRename := os.Rename(databaseDir, failedDir); errRename != nil {
		t.Fatalf("rename failed database directory after Open() error: %v", errRename)
	}
	replacement, errReplacement := carpoolsqlite.Open(ctx, carpoolsqlite.Config{Path: databasePath})
	if errReplacement != nil {
		t.Fatalf("open replacement database after initialization failure: %v", errReplacement)
	}
	if errClose := replacement.Close(); errClose != nil {
		t.Fatalf("replacement Close() error = %v", errClose)
	}
}

func moduleTestConfig(databasePath string, enabled bool) *config.Config {
	carpoolConfig := config.DefaultCarpoolConfig()
	carpoolConfig.Enabled = enabled
	carpoolConfig.DatabasePath = databasePath
	return &config.Config{Carpool: carpoolConfig}
}

func moduleTestPaths(t *testing.T) (root, databasePath, configPath string) {
	t.Helper()
	root = t.TempDir()
	reportedTemp := filepath.Join(root, "reported-temp")
	t.Setenv("TMPDIR", reportedTemp)
	t.Setenv("TMP", reportedTemp)
	t.Setenv("TEMP", reportedTemp)
	return root, filepath.Join(root, "database", "carpool.db"), filepath.Join(root, "config.yaml")
}

func assertNoOpenDatabaseDescriptors(t *testing.T, databasePath string) {
	t.Helper()
	entries, errReadDir := os.ReadDir("/proc/self/fd")
	if errReadDir != nil {
		return
	}
	wanted := map[string]struct{}{
		databasePath:          {},
		databasePath + "-shm": {},
		databasePath + "-wal": {},
	}
	for _, entry := range entries {
		target, errReadlink := os.Readlink(filepath.Join("/proc/self/fd", entry.Name()))
		if errReadlink != nil {
			continue
		}
		target = strings.TrimSuffix(target, " (deleted)")
		if _, open := wanted[target]; open {
			t.Fatalf("database descriptor remained open after initialization failure: %s", target)
		}
	}
}

type moduleLogHook struct {
	entries []*log.Entry
}

func (h *moduleLogHook) Levels() []log.Level { return log.AllLevels }

func (h *moduleLogHook) Fire(entry *log.Entry) error {
	clone := *entry
	clone.Data = make(log.Fields, len(entry.Data))
	for key, value := range entry.Data {
		clone.Data[key] = value
	}
	h.entries = append(h.entries, &clone)
	return nil
}
