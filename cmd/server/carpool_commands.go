package main

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/charmbracelet/x/term"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/buildinfo"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/ops"
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type carpoolCommandOptions struct {
	bootstrapAdmin string
	backupPath     string
	restorePath    string
	bootstrapSet   bool
	backupSet      bool
	restoreSet     bool
}

func (options carpoolCommandOptions) requested() bool {
	return options.commandCount() > 0
}

func (options carpoolCommandOptions) commandCount() int {
	count := 0
	for _, requested := range []bool{options.bootstrapRequested(), options.backupRequested(), options.restoreRequested()} {
		if requested {
			count++
		}
	}
	return count
}

func (options carpoolCommandOptions) bootstrapRequested() bool {
	return options.bootstrapSet || options.bootstrapAdmin != ""
}

func (options carpoolCommandOptions) backupRequested() bool {
	return options.backupSet || options.backupPath != ""
}

func (options carpoolCommandOptions) restoreRequested() bool {
	return options.restoreSet || options.restorePath != ""
}

type carpoolStringFlag struct {
	value string
	set   bool
}

func (value *carpoolStringFlag) String() string {
	if value == nil {
		return ""
	}
	return value.value
}

func (value *carpoolStringFlag) Set(raw string) error {
	value.value = raw
	value.set = true
	return nil
}

type carpoolPasswordTerminal interface {
	IsTerminal() bool
	ReadPassword() ([]byte, error)
}

type standardCarpoolTerminal struct {
	input *os.File
}

func (terminalInput standardCarpoolTerminal) IsTerminal() bool {
	return terminalInput.input != nil && term.IsTerminal(terminalInput.input.Fd())
}

func (terminalInput standardCarpoolTerminal) ReadPassword() ([]byte, error) {
	if terminalInput.input == nil {
		return nil, fmt.Errorf("carpool bootstrap: terminal input is unavailable")
	}
	return term.ReadPassword(terminalInput.input.Fd())
}

type carpoolCommandDependencies struct {
	terminal            carpoolPasswordTerminal
	output              io.Writer
	now                 func() time.Time
	passwordHasher      carpoolservice.PasswordHasher
	resolveDatabasePath func(config.CarpoolConfig, string) (string, error)
}

func defaultCarpoolCommandDependencies() carpoolCommandDependencies {
	return carpoolCommandDependencies{
		terminal: standardCarpoolTerminal{input: os.Stdin},
		output:   os.Stdout,
		now:      time.Now,
	}
}

func carpoolCommandRequestedInArgs(args []string) bool {
	for _, argument := range args {
		if argument == "--" {
			return false
		}
		for _, name := range []string{"carpool-bootstrap-admin", "carpool-backup", "carpool-restore"} {
			if argument == "-"+name || argument == "--"+name ||
				strings.HasPrefix(argument, "-"+name+"=") || strings.HasPrefix(argument, "--"+name+"=") {
				return true
			}
		}
	}
	return false
}

func runCarpoolCommand(
	ctx context.Context,
	cfg *config.Config,
	configPath string,
	command carpoolCommandOptions,
	dependencies carpoolCommandDependencies,
) (bool, error) {
	commandCount := command.commandCount()
	if commandCount == 0 {
		return false, nil
	}
	if commandCount != 1 {
		return true, fmt.Errorf("carpool command: specify exactly one of --carpool-bootstrap-admin, --carpool-backup, or --carpool-restore")
	}
	if cfg == nil || !cfg.Carpool.Enabled {
		return true, fmt.Errorf("carpool command: carpool.enabled must be true")
	}
	if errValidate := cfg.ValidateCarpool(); errValidate != nil {
		return true, fmt.Errorf("carpool command: validate configuration: %w", errValidate)
	}
	resolveDatabasePath := dependencies.resolveDatabasePath
	if resolveDatabasePath == nil {
		resolveDatabasePath = func(carpoolConfig config.CarpoolConfig, loadedConfigPath string) (string, error) {
			return carpoolConfig.DatabasePathForConfig(loadedConfigPath)
		}
	}
	databasePath, errPath := resolveDatabasePath(cfg.Carpool, configPath)
	if errPath != nil {
		return true, fmt.Errorf("carpool command: resolve database path: %w", errPath)
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if dependencies.output == nil {
		dependencies.output = io.Discard
	}
	if dependencies.now == nil {
		dependencies.now = time.Now
	}

	switch {
	case command.bootstrapRequested():
		if _, errUsername := carpoolservice.NormalizeUsername(command.bootstrapAdmin); errUsername != nil {
			return true, errUsername
		}
		if errStopped := ops.RequireStopped(databasePath); errStopped != nil {
			return true, errStopped
		}
		password, errPassword := readConfirmedCarpoolPassword(dependencies.terminal, dependencies.output)
		if errPassword != nil {
			return true, errPassword
		}
		user, errBootstrap := bootstrapCarpoolAdmin(ctx, cfg, databasePath, command.bootstrapAdmin, password, dependencies.passwordHasher)
		if errBootstrap != nil {
			return true, errBootstrap
		}
		if _, errWrite := fmt.Fprintf(dependencies.output, "Carpool administrator %q created.\n", user.Username); errWrite != nil {
			return true, fmt.Errorf("carpool bootstrap: write confirmation: %w", errWrite)
		}
	case command.backupRequested():
		manifest, errBackup := ops.BackupStopped(ctx, ops.BackupOptions{
			DatabasePath:  databasePath,
			BackupPath:    command.backupPath,
			BinaryVersion: buildinfo.Version,
			Now:           dependencies.now,
		})
		if errBackup != nil {
			return true, errBackup
		}
		if _, errWrite := fmt.Fprintf(dependencies.output, "Carpool backup created at %q (schema %d, sha256 %s).\n",
			command.backupPath, manifest.SchemaVersion, manifest.DatabaseSHA256); errWrite != nil {
			return true, fmt.Errorf("carpool backup: write confirmation: %w", errWrite)
		}
	case command.restoreRequested():
		manifest, errRestore := ops.RestoreStopped(ctx, databasePath, command.restorePath)
		if errRestore != nil {
			return true, errRestore
		}
		if _, errWrite := fmt.Fprintf(dependencies.output, "Carpool database restored from %q (schema %d).\n",
			command.restorePath, manifest.SchemaVersion); errWrite != nil {
			return true, fmt.Errorf("carpool restore: write confirmation: %w", errWrite)
		}
	}
	return true, nil
}

func readConfirmedCarpoolPassword(terminalInput carpoolPasswordTerminal, output io.Writer) (string, error) {
	if terminalInput == nil || !terminalInput.IsTerminal() {
		return "", fmt.Errorf("carpool bootstrap: password input requires a TTY")
	}
	if output == nil {
		output = io.Discard
	}
	first, errFirst := readCarpoolPassword(terminalInput, output, "Enter password: ")
	if errFirst != nil {
		return "", errFirst
	}
	defer clearBytes(first)
	second, errSecond := readCarpoolPassword(terminalInput, output, "Confirm password: ")
	if errSecond != nil {
		return "", errSecond
	}
	defer clearBytes(second)
	if subtle.ConstantTimeCompare(first, second) != 1 {
		return "", fmt.Errorf("carpool bootstrap: passwords do not match")
	}
	password := string(first)
	if errValidate := carpoolservice.ValidatePassword(password); errValidate != nil {
		return "", errValidate
	}
	return password, nil
}

func readCarpoolPassword(terminalInput carpoolPasswordTerminal, output io.Writer, prompt string) ([]byte, error) {
	if _, errWrite := io.WriteString(output, prompt); errWrite != nil {
		return nil, fmt.Errorf("carpool bootstrap: write password prompt: %w", errWrite)
	}
	password, errRead := terminalInput.ReadPassword()
	_, errNewline := io.WriteString(output, "\n")
	if errRead != nil {
		clearBytes(password)
		return nil, fmt.Errorf("carpool bootstrap: read password: %w", errRead)
	}
	if errNewline != nil {
		clearBytes(password)
		return nil, fmt.Errorf("carpool bootstrap: write password prompt: %w", errNewline)
	}
	return password, nil
}

func bootstrapCarpoolAdmin(
	ctx context.Context,
	cfg *config.Config,
	databasePath string,
	username string,
	password string,
	passwordHasher carpoolservice.PasswordHasher,
) (user domain.User, err error) {
	store, errOpen := carpoolsqlite.Open(ctx, carpoolsqlite.Config{Path: databasePath})
	if errOpen != nil {
		return domain.User{}, fmt.Errorf("carpool bootstrap: open database: %w", errOpen)
	}
	defer func() {
		if errClose := store.Close(); errClose != nil {
			err = errors.Join(err, fmt.Errorf("carpool bootstrap: close database: %w", errClose))
		}
	}()
	absoluteTTL, idleTTL, errDurations := cfg.Carpool.SessionDurations()
	if errDurations != nil {
		return domain.User{}, fmt.Errorf("carpool bootstrap: parse session lifetimes: %w", errDurations)
	}
	reportLocation, errLocation := time.LoadLocation(cfg.Carpool.ReportTimezone)
	if errLocation != nil {
		return domain.User{}, fmt.Errorf("carpool bootstrap: load report timezone: %w", errLocation)
	}
	control, errControl := carpoolservice.NewControl(store, nil, carpoolservice.ControlConfig{
		SessionAbsoluteTTL: absoluteTTL,
		SessionIdleTTL:     idleTTL,
		ReportLocation:     reportLocation,
		UsageRetention:     time.Duration(cfg.Carpool.UsageRetentionDays) * 24 * time.Hour,
		HomeEnabled:        cfg.Home.Enabled,
		PasswordHasher:     passwordHasher,
	})
	if errControl != nil {
		return domain.User{}, fmt.Errorf("carpool bootstrap: create service: %w", errControl)
	}
	user, errBootstrap := control.BootstrapAdmin(ctx, username, username, password)
	if errBootstrap != nil {
		return domain.User{}, fmt.Errorf("carpool bootstrap: create administrator: %w", errBootstrap)
	}
	return user, nil
}

func clearBytes(value []byte) {
	for index := range value {
		value[index] = 0
	}
}
