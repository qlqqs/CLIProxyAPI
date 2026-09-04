package config

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	CarpoolAPIKeyPrefix              = "cpk_v1_"
	DefaultCarpoolDatabasePath       = "./data/carpool.db"
	DefaultCarpoolReportTimezone     = "UTC"
	DefaultCarpoolUsageRetention     = 90
	DefaultCarpoolAuditRetention     = 180
	DefaultCarpoolSessionAbsoluteTTL = "24h"
	DefaultCarpoolSessionIdleTTL     = "2h"
)

// CarpoolConfig configures the optional carpool user-management module.
type CarpoolConfig struct {
	Enabled            bool                 `yaml:"enabled" json:"enabled"`
	DatabasePath       string               `yaml:"database-path" json:"database-path"`
	ReportTimezone     string               `yaml:"report-timezone" json:"report-timezone"`
	UsageRetentionDays int                  `yaml:"usage-retention-days" json:"usage-retention-days"`
	AuditRetentionDays int                  `yaml:"audit-retention-days" json:"audit-retention-days"`
	Session            CarpoolSessionConfig `yaml:"session" json:"session"`
	TrustedOrigins     []string             `yaml:"trusted-origins" json:"trusted-origins"`
	TrustedProxyCIDRs  []string             `yaml:"trusted-proxy-cidrs" json:"trusted-proxy-cidrs"`
}

// CarpoolSessionConfig configures browser session behavior for the carpool UI.
type CarpoolSessionConfig struct {
	AbsoluteTTL  string `yaml:"absolute-ttl" json:"absolute-ttl"`
	IdleTTL      string `yaml:"idle-ttl" json:"idle-ttl"`
	CookieSecure bool   `yaml:"cookie-secure" json:"cookie-secure"`
}

// DefaultCarpoolConfig returns the documented carpool defaults.
func DefaultCarpoolConfig() CarpoolConfig {
	return CarpoolConfig{
		DatabasePath:       DefaultCarpoolDatabasePath,
		ReportTimezone:     DefaultCarpoolReportTimezone,
		UsageRetentionDays: DefaultCarpoolUsageRetention,
		AuditRetentionDays: DefaultCarpoolAuditRetention,
		Session: CarpoolSessionConfig{
			AbsoluteTTL:  DefaultCarpoolSessionAbsoluteTTL,
			IdleTTL:      DefaultCarpoolSessionIdleTTL,
			CookieSecure: true,
		},
	}
}

// ValidateCarpool validates enabled carpool settings and reserved API key usage.
func (cfg *Config) ValidateCarpool() error {
	if cfg == nil || !cfg.Carpool.Enabled {
		return nil
	}
	if errValidate := cfg.Carpool.Validate(); errValidate != nil {
		return errValidate
	}
	for _, apiKey := range cfg.APIKeys {
		if strings.HasPrefix(strings.TrimSpace(apiKey), CarpoolAPIKeyPrefix) {
			return fmt.Errorf("carpool: api-keys cannot use reserved prefix %q", CarpoolAPIKeyPrefix)
		}
	}
	return nil
}

// Validate validates enabled carpool settings without resolving a relative database path.
func (cfg CarpoolConfig) Validate() error {
	if !cfg.Enabled {
		return nil
	}
	if strings.TrimSpace(cfg.DatabasePath) == "" {
		return fmt.Errorf("carpool: database-path is required")
	}
	if strings.TrimSpace(cfg.ReportTimezone) == "" {
		return fmt.Errorf("carpool: report-timezone is required")
	}
	if _, errLocation := time.LoadLocation(strings.TrimSpace(cfg.ReportTimezone)); errLocation != nil {
		return fmt.Errorf("carpool: invalid report-timezone: %w", errLocation)
	}
	if cfg.UsageRetentionDays < 30 {
		return fmt.Errorf("carpool: usage-retention-days must be at least 30")
	}
	if cfg.AuditRetentionDays <= 0 {
		return fmt.Errorf("carpool: audit-retention-days must be positive")
	}
	absoluteTTL, errAbsolute := parsePositiveCarpoolDuration("session.absolute-ttl", cfg.Session.AbsoluteTTL)
	if errAbsolute != nil {
		return errAbsolute
	}
	idleTTL, errIdle := parsePositiveCarpoolDuration("session.idle-ttl", cfg.Session.IdleTTL)
	if errIdle != nil {
		return errIdle
	}
	if idleTTL > absoluteTTL {
		return fmt.Errorf("carpool: session.idle-ttl cannot exceed session.absolute-ttl")
	}
	for _, rawOrigin := range cfg.TrustedOrigins {
		if errOrigin := validateCarpoolOrigin(rawOrigin); errOrigin != nil {
			return errOrigin
		}
	}
	for _, rawCIDR := range cfg.TrustedProxyCIDRs {
		if _, _, errCIDR := net.ParseCIDR(strings.TrimSpace(rawCIDR)); errCIDR != nil {
			return fmt.Errorf("carpool: invalid trusted-proxy-cidrs entry %q: %w", rawCIDR, errCIDR)
		}
	}
	return nil
}

// SessionDurations parses the configured absolute and idle session lifetimes.
func (cfg CarpoolConfig) SessionDurations() (time.Duration, time.Duration, error) {
	absoluteTTL, errAbsolute := parsePositiveCarpoolDuration("session.absolute-ttl", cfg.Session.AbsoluteTTL)
	if errAbsolute != nil {
		return 0, 0, errAbsolute
	}
	idleTTL, errIdle := parsePositiveCarpoolDuration("session.idle-ttl", cfg.Session.IdleTTL)
	if errIdle != nil {
		return 0, 0, errIdle
	}
	return absoluteTTL, idleTTL, nil
}

// DatabasePathForConfig resolves a relative database path from the config file directory.
func (cfg CarpoolConfig) DatabasePathForConfig(configFile string) (string, error) {
	rawPath := strings.TrimSpace(cfg.DatabasePath)
	if rawPath == "" {
		return "", fmt.Errorf("carpool: database-path is required")
	}
	resolvedPath := rawPath
	if !filepath.IsAbs(resolvedPath) {
		if strings.TrimSpace(configFile) == "" {
			return "", fmt.Errorf("carpool: config path is required to resolve relative database-path")
		}
		configPath, errAbs := filepath.Abs(configFile)
		if errAbs != nil {
			return "", fmt.Errorf("carpool: resolve config path: %w", errAbs)
		}
		resolvedPath = filepath.Join(filepath.Dir(configPath), resolvedPath)
	}
	resolvedPath, errAbs := filepath.Abs(filepath.Clean(resolvedPath))
	if errAbs != nil {
		return "", fmt.Errorf("carpool: resolve database-path: %w", errAbs)
	}
	tempDir, errTemp := filepath.Abs(os.TempDir())
	if errTemp == nil && pathWithinDirectory(resolvedPath, tempDir) {
		return "", fmt.Errorf("carpool: database-path cannot be inside the temporary directory")
	}
	return resolvedPath, nil
}

func parsePositiveCarpoolDuration(name, raw string) (time.Duration, error) {
	duration, errParse := time.ParseDuration(strings.TrimSpace(raw))
	if errParse != nil || duration <= 0 {
		if errParse != nil {
			return 0, fmt.Errorf("carpool: invalid %s: %w", name, errParse)
		}
		return 0, fmt.Errorf("carpool: %s must be positive", name)
	}
	return duration, nil
}

func validateCarpoolOrigin(raw string) error {
	origin := strings.TrimSpace(raw)
	parsed, errParse := url.Parse(origin)
	if errParse != nil || parsed == nil {
		return fmt.Errorf("carpool: invalid trusted-origins entry %q", raw)
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		return fmt.Errorf("carpool: trusted origin %q must use http or https", raw)
	}
	if parsed.Host == "" || parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return fmt.Errorf("carpool: trusted origin %q must contain only scheme and authority", raw)
	}
	return nil
}

func pathWithinDirectory(path, directory string) bool {
	relative, errRel := filepath.Rel(directory, path)
	if errRel != nil {
		return false
	}
	return relative == "." || (relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator)))
}
