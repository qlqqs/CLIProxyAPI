package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestParseConfigBytesCarpoolDefaults(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte("{}"))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if !reflect.DeepEqual(cfg.Carpool, DefaultCarpoolConfig()) {
		t.Fatalf("Carpool = %#v, want %#v", cfg.Carpool, DefaultCarpoolConfig())
	}
}

func TestParseConfigBytesCarpoolEnabled(t *testing.T) {
	cfg, errParse := ParseConfigBytes([]byte(`
carpool:
  enabled: true
  database-path: /var/lib/cli-proxy-api/carpool.db
  report-timezone: Asia/Shanghai
  usage-retention-days: 45
  audit-retention-days: 200
  session:
    absolute-ttl: 12h
    idle-ttl: 30m
    cookie-secure: false
  trusted-origins:
    - https://proxy.example.com
  trusted-proxy-cidrs:
    - 10.0.0.0/8
`))
	if errParse != nil {
		t.Fatalf("ParseConfigBytes() error = %v", errParse)
	}
	if !cfg.Carpool.Enabled || cfg.Carpool.ReportTimezone != "Asia/Shanghai" {
		t.Fatalf("Carpool = %#v", cfg.Carpool)
	}
	absoluteTTL, idleTTL, errDurations := cfg.Carpool.SessionDurations()
	if errDurations != nil {
		t.Fatalf("SessionDurations() error = %v", errDurations)
	}
	if absoluteTTL != 12*time.Hour || idleTTL != 30*time.Minute {
		t.Fatalf("SessionDurations() = (%s, %s)", absoluteTTL, idleTTL)
	}
}

func TestCarpoolConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Config)
		want   string
	}{
		{name: "valid", mutate: func(*Config) {}},
		{name: "missing database", mutate: func(cfg *Config) { cfg.Carpool.DatabasePath = "" }, want: "database-path"},
		{name: "invalid timezone", mutate: func(cfg *Config) { cfg.Carpool.ReportTimezone = "Mars/Olympus" }, want: "report-timezone"},
		{name: "short retention", mutate: func(cfg *Config) { cfg.Carpool.UsageRetentionDays = 29 }, want: "usage-retention-days"},
		{name: "invalid audit retention", mutate: func(cfg *Config) { cfg.Carpool.AuditRetentionDays = 0 }, want: "audit-retention-days"},
		{name: "invalid absolute ttl", mutate: func(cfg *Config) { cfg.Carpool.Session.AbsoluteTTL = "soon" }, want: "absolute-ttl"},
		{name: "idle exceeds absolute", mutate: func(cfg *Config) { cfg.Carpool.Session.IdleTTL = "25h" }, want: "idle-ttl"},
		{name: "origin has path", mutate: func(cfg *Config) { cfg.Carpool.TrustedOrigins = []string{"https://example.com/path"} }, want: "trusted origin"},
		{name: "invalid cidr", mutate: func(cfg *Config) { cfg.Carpool.TrustedProxyCIDRs = []string{"10.0.0.1"} }, want: "trusted-proxy-cidrs"},
		{name: "reserved global key", mutate: func(cfg *Config) { cfg.APIKeys = []string{"cpk_v1_collision.secret"} }, want: "reserved prefix"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := &Config{Carpool: DefaultCarpoolConfig()}
			cfg.Carpool.Enabled = true
			test.mutate(cfg)
			errValidate := cfg.ValidateCarpool()
			if test.want == "" {
				if errValidate != nil {
					t.Fatalf("ValidateCarpool() error = %v", errValidate)
				}
				return
			}
			if errValidate == nil || !strings.Contains(errValidate.Error(), test.want) {
				t.Fatalf("ValidateCarpool() error = %v, want containing %q", errValidate, test.want)
			}
		})
	}
}

func TestCarpoolDatabasePathForConfig(t *testing.T) {
	configPath := filepath.Join(string(filepath.Separator), "srv", "cli-proxy-api", "config.yaml")
	cfg := DefaultCarpoolConfig()
	got, errResolve := cfg.DatabasePathForConfig(configPath)
	if errResolve != nil {
		t.Fatalf("DatabasePathForConfig() error = %v", errResolve)
	}
	want := filepath.Join(filepath.Dir(configPath), "data", "carpool.db")
	if got != want {
		t.Fatalf("DatabasePathForConfig() = %q, want %q", got, want)
	}
}

func TestDisabledCarpoolIgnoresDormantValues(t *testing.T) {
	cfg := &Config{Carpool: CarpoolConfig{Enabled: false, DatabasePath: ""}, SDKConfig: SDKConfig{APIKeys: []string{"cpk_v1_legacy"}}}
	if errValidate := cfg.ValidateCarpool(); errValidate != nil {
		t.Fatalf("ValidateCarpool() error = %v", errValidate)
	}
}

func TestSaveConfigPreserveCommentsPreservesDisabledCarpool(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	if errWrite := os.WriteFile(configPath, []byte("debug: true\n"), 0o600); errWrite != nil {
		t.Fatalf("os.WriteFile() error = %v", errWrite)
	}
	carpool := DefaultCarpoolConfig()
	carpool.Enabled = false
	cfg := &Config{Debug: true, Carpool: carpool}
	if errSave := SaveConfigPreserveComments(configPath, cfg); errSave != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", errSave)
	}
	data, errRead := os.ReadFile(configPath)
	if errRead != nil {
		t.Fatalf("os.ReadFile() error = %v", errRead)
	}
	if !strings.Contains(string(data), "enabled: false") {
		t.Fatalf("saved config dropped explicit carpool disable:\n%s", data)
	}
}
