package config

import (
	"encoding/json"
	"fmt"
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
		{name: "zero queue capacity", mutate: func(cfg *Config) { cfg.Carpool.ConcurrencyQueueCapacity = 0 }, want: "concurrency-queue-capacity"},
		{name: "negative queue capacity", mutate: func(cfg *Config) { cfg.Carpool.ConcurrencyQueueCapacity = -1 }, want: "concurrency-queue-capacity"},
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

func TestCarpoolConcurrencyQueueCapacityParsing(t *testing.T) {
	for _, test := range []struct {
		name    string
		yaml    string
		want    int
		invalid bool
	}{
		{name: "omitted", yaml: "carpool: {}", want: 10},
		{name: "null retains default", yaml: "carpool: {concurrency-queue-capacity: null}", want: 10},
		{name: "minimum", yaml: "carpool: {concurrency-queue-capacity: 1}", want: 1},
		{name: "custom", yaml: "carpool: {concurrency-queue-capacity: 25}", want: 25},
		{name: "explicit zero", yaml: "carpool: {concurrency-queue-capacity: 0}", invalid: true},
		{name: "negative", yaml: "carpool: {concurrency-queue-capacity: -5}", invalid: true},
		{name: "fractional", yaml: "carpool: {concurrency-queue-capacity: 1.5}", invalid: true},
		{name: "merged fractional", yaml: "defaults: &defaults {concurrency-queue-capacity: 1.5}\ncarpool: {<<: *defaults}", invalid: true},
		{name: "merged integer", yaml: "defaults: &defaults {concurrency-queue-capacity: 13}\ncarpool: {<<: *defaults}", want: 13},
		{name: "invalid string", yaml: "carpool: {concurrency-queue-capacity: many}", invalid: true},
		{name: "disabled", yaml: "carpool: {enabled: false, concurrency-queue-capacity: 0}", want: 0},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(test.yaml))
			if test.invalid {
				if errParse == nil {
					t.Fatal("invalid queue capacity accepted")
				}
				return
			}
			if errParse != nil {
				t.Fatal(errParse)
			}
			if cfg.Carpool.ConcurrencyQueueCapacity != test.want {
				t.Fatalf("queue capacity = %d, want %d", cfg.Carpool.ConcurrencyQueueCapacity, test.want)
			}
		})
	}
}

func TestCarpoolConcurrencyQueueCapacityRoundTrip(t *testing.T) {
	cfg := DefaultCarpoolConfig()
	cfg.ConcurrencyQueueCapacity = 17
	encoded, errMarshal := json.Marshal(cfg)
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	var decoded CarpoolConfig
	if errDecode := json.Unmarshal(encoded, &decoded); errDecode != nil {
		t.Fatal(errDecode)
	}
	if decoded.ConcurrencyQueueCapacity != 17 || !strings.Contains(string(encoded), `"concurrency-queue-capacity":17`) {
		t.Fatalf("JSON queue capacity round trip = %s", encoded)
	}
	for _, capacity := range []int{DefaultCarpoolConcurrencyQueueCapacity, 17} {
		t.Run(fmt.Sprint(capacity), func(t *testing.T) {
			configPath := filepath.Join(t.TempDir(), "config.yaml")
			if errWrite := os.WriteFile(configPath, []byte("# Preserve this comment\ncarpool:\n  enabled: true\n"), 0o600); errWrite != nil {
				t.Fatal(errWrite)
			}
			cfg.ConcurrencyQueueCapacity = capacity
			if errSave := SaveConfigPreserveComments(configPath, &Config{Carpool: cfg}); errSave != nil {
				t.Fatal(errSave)
			}
			data, errRead := os.ReadFile(configPath)
			if errRead != nil {
				t.Fatal(errRead)
			}
			if !strings.Contains(string(data), "# Preserve this comment") {
				t.Fatalf("lost config comment: %s", data)
			}
			loaded, errLoad := LoadConfig(configPath)
			if errLoad != nil {
				t.Fatal(errLoad)
			}
			if loaded.Carpool.ConcurrencyQueueCapacity != capacity {
				t.Fatalf("saved queue capacity = %d, want %d", loaded.Carpool.ConcurrencyQueueCapacity, capacity)
			}
		})
	}
}
