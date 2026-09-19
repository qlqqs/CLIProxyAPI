package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/domain"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/ops"
	carpoolservice "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/service"
	carpoolsqlite "github.com/router-for-me/CLIProxyAPI/v7/internal/carpool/store/sqlite"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

type fakeCarpoolTerminal struct {
	terminal  bool
	passwords [][]byte
	readError error
	reads     int
}

func (terminalInput *fakeCarpoolTerminal) IsTerminal() bool {
	return terminalInput != nil && terminalInput.terminal
}

func (terminalInput *fakeCarpoolTerminal) ReadPassword() ([]byte, error) {
	terminalInput.reads++
	if terminalInput.readError != nil {
		return nil, terminalInput.readError
	}
	if len(terminalInput.passwords) == 0 {
		return nil, fmt.Errorf("no password input")
	}
	password := append([]byte(nil), terminalInput.passwords[0]...)
	terminalInput.passwords = terminalInput.passwords[1:]
	return password, nil
}

func TestReadConfirmedCarpoolPasswordRequiresTTY(t *testing.T) {
	terminalInput := &fakeCarpoolTerminal{terminal: false, passwords: [][]byte{[]byte("not-read-password")}}
	var output bytes.Buffer

	_, errPassword := readConfirmedCarpoolPassword(terminalInput, &output)
	if errPassword == nil || !strings.Contains(errPassword.Error(), "requires a TTY") {
		t.Fatalf("readConfirmedCarpoolPassword() error = %v, want TTY error", errPassword)
	}
	if terminalInput.reads != 0 {
		t.Fatalf("ReadPassword() calls = %d, want 0", terminalInput.reads)
	}
	if output.Len() != 0 {
		t.Fatalf("output = %q, want empty", output.String())
	}
}

func TestReadConfirmedCarpoolPassword(t *testing.T) {
	const password = "correct horse battery staple"
	terminalInput := &fakeCarpoolTerminal{
		terminal:  true,
		passwords: [][]byte{[]byte(password), []byte(password)},
	}
	var output bytes.Buffer

	got, errPassword := readConfirmedCarpoolPassword(terminalInput, &output)
	if errPassword != nil {
		t.Fatalf("readConfirmedCarpoolPassword() error = %v", errPassword)
	}
	if got != password {
		t.Fatal("readConfirmedCarpoolPassword() did not return the entered password")
	}
	if terminalInput.reads != 2 {
		t.Fatalf("ReadPassword() calls = %d, want 2", terminalInput.reads)
	}
	if strings.Contains(output.String(), password) {
		t.Fatal("password appeared in terminal output")
	}
	if !strings.Contains(output.String(), "Enter password") || !strings.Contains(output.String(), "Confirm password") {
		t.Fatalf("terminal output = %q, want both prompts", output.String())
	}
}

func TestReadConfirmedCarpoolPasswordRejectsMismatchWithoutLeaking(t *testing.T) {
	const first = "first-password-value"
	const second = "second-password-value"
	terminalInput := &fakeCarpoolTerminal{
		terminal:  true,
		passwords: [][]byte{[]byte(first), []byte(second)},
	}
	var output bytes.Buffer

	_, errPassword := readConfirmedCarpoolPassword(terminalInput, &output)
	if errPassword == nil || !strings.Contains(errPassword.Error(), "do not match") {
		t.Fatalf("readConfirmedCarpoolPassword() error = %v, want mismatch", errPassword)
	}
	diagnostic := output.String() + errPassword.Error()
	if strings.Contains(diagnostic, first) || strings.Contains(diagnostic, second) {
		t.Fatal("mismatched password appeared in output or error")
	}
}

func TestRunCarpoolBootstrapAdmin(t *testing.T) {
	cfg, configPath, databasePath := enabledCarpoolCommandConfig(t)
	const password = "bootstrap-password-value"
	terminalInput := matchingTerminal(password)
	var output bytes.Buffer

	handled, errRun := runCarpoolCommand(context.Background(), cfg, configPath, carpoolCommandOptions{
		bootstrapAdmin: "First.Admin",
	}, commandDependencies(databasePath, terminalInput, &output))
	if !handled || errRun != nil {
		t.Fatalf("runCarpoolCommand() = (%t, %v), want (true, nil)", handled, errRun)
	}
	if strings.Contains(output.String(), password) {
		t.Fatal("bootstrap password appeared in command output")
	}

	store := openCommandDatabase(t, databasePath)
	user, errUser := store.GetUserByNormalizedUsername(context.Background(), "first.admin")
	if errUser != nil {
		closeCommandDatabase(t, store)
		t.Fatalf("GetUserByNormalizedUsername() error = %v", errUser)
	}
	closeCommandDatabase(t, store)
	if user.Role != domain.UserRoleAdmin || user.Status != domain.UserStatusActive {
		t.Fatalf("bootstrap user role/status = %q/%q", user.Role, user.Status)
	}
	if user.PasswordHash == "" || strings.Contains(user.PasswordHash, password) {
		t.Fatal("database did not store a safe password hash")
	}
	verified, errVerify := fastPasswordHasher().Verify(user.PasswordHash, password)
	if errVerify != nil || !verified {
		t.Fatalf("Verify() = (%t, %v), want (true, nil)", verified, errVerify)
	}
	databaseContents, errRead := os.ReadFile(databasePath)
	if errRead != nil {
		t.Fatalf("read database: %v", errRead)
	}
	if bytes.Contains(databaseContents, []byte(password)) {
		t.Fatal("plaintext password appeared in database bytes")
	}
}

func TestRunCarpoolBootstrapRandomAdminIsIdempotent(t *testing.T) {
	cfg, configPath, databasePath := enabledCarpoolCommandConfig(t)
	const password = "generated-bootstrap-password-value"
	command := carpoolCommandOptions{randomBootstrapAdmin: "admin"}
	dependencies := commandDependencies(databasePath, nil, nil)
	dependencies.generatePassword = func() (string, error) { return password, nil }
	var firstOutput bytes.Buffer
	dependencies.output = &firstOutput

	handled, errRun := runCarpoolCommand(context.Background(), cfg, configPath, command, dependencies)
	if !handled || errRun != nil {
		t.Fatalf("first runCarpoolCommand() = (%t, %v)", handled, errRun)
	}
	if !strings.Contains(firstOutput.String(), `administrator "admin" created`) || !strings.Contains(firstOutput.String(), password) {
		t.Fatalf("first output = %q, want generated credentials", firstOutput.String())
	}

	var secondOutput bytes.Buffer
	dependencies.output = &secondOutput
	handled, errRun = runCarpoolCommand(context.Background(), cfg, configPath, command, dependencies)
	if !handled || errRun != nil {
		t.Fatalf("second runCarpoolCommand() = (%t, %v)", handled, errRun)
	}
	if !strings.Contains(secondOutput.String(), "already exists") || strings.Contains(secondOutput.String(), password) {
		t.Fatalf("second output = %q, want idempotent skip without password", secondOutput.String())
	}
}

func TestRunCarpoolBootstrapAdminRejectsRepeat(t *testing.T) {
	cfg, configPath, databasePath := enabledCarpoolCommandConfig(t)
	const password = "repeat-bootstrap-password"
	command := carpoolCommandOptions{bootstrapAdmin: "admin"}
	dependencies := func() carpoolCommandDependencies {
		return commandDependencies(databasePath, matchingTerminal(password), io.Discard)
	}
	if handled, errRun := runCarpoolCommand(context.Background(), cfg, configPath, command, dependencies()); !handled || errRun != nil {
		t.Fatalf("first runCarpoolCommand() = (%t, %v)", handled, errRun)
	}
	handled, errRun := runCarpoolCommand(context.Background(), cfg, configPath, command, dependencies())
	if !handled || !errors.Is(errRun, domain.ErrAlreadyBootstrapped) {
		t.Fatalf("second runCarpoolCommand() = (%t, %v), want ErrAlreadyBootstrapped", handled, errRun)
	}
	if strings.Contains(errRun.Error(), password) {
		t.Fatal("bootstrap password appeared in repeat error")
	}
}

func TestRunCarpoolCommandRejectsDisabledAndConflictingModes(t *testing.T) {
	t.Run("disabled", func(t *testing.T) {
		directory := t.TempDir()
		cfg := &config.Config{Carpool: config.DefaultCarpoolConfig()}
		cfg.Carpool.Enabled = false
		cfg.Carpool.DatabasePath = "./data/carpool.db"
		configPath := filepath.Join(directory, "config.yaml")
		databasePath := filepath.Join(directory, "data", "carpool.db")
		terminalInput := matchingTerminal("disabled-password-value")

		handled, errRun := runCarpoolCommand(context.Background(), cfg, configPath,
			carpoolCommandOptions{bootstrapAdmin: "admin"},
			carpoolCommandDependencies{terminal: terminalInput})
		if !handled || errRun == nil || !strings.Contains(errRun.Error(), "carpool.enabled") {
			t.Fatalf("runCarpoolCommand() = (%t, %v), want disabled error", handled, errRun)
		}
		if terminalInput.reads != 0 {
			t.Fatalf("ReadPassword() calls = %d, want 0", terminalInput.reads)
		}
		if _, errStat := os.Stat(databasePath); !errors.Is(errStat, os.ErrNotExist) {
			t.Fatalf("database stat error = %v, want not exist", errStat)
		}
	})

	t.Run("conflicting commands", func(t *testing.T) {
		cfg, configPath, databasePath := enabledCarpoolCommandConfig(t)
		terminalInput := matchingTerminal("conflicting-password-value")
		handled, errRun := runCarpoolCommand(context.Background(), cfg, configPath, carpoolCommandOptions{
			bootstrapAdmin: "admin",
			backupPath:     filepath.Join(t.TempDir(), "backup.db"),
		}, commandDependencies(databasePath, terminalInput, io.Discard))
		if !handled || errRun == nil || !strings.Contains(errRun.Error(), "exactly one") {
			t.Fatalf("runCarpoolCommand() = (%t, %v), want conflict error", handled, errRun)
		}
		if terminalInput.reads != 0 {
			t.Fatalf("ReadPassword() calls = %d, want 0", terminalInput.reads)
		}
		if _, errStat := os.Stat(databasePath); !errors.Is(errStat, os.ErrNotExist) {
			t.Fatalf("database stat error = %v, want not exist", errStat)
		}
	})

	t.Run("explicit empty bootstrap", func(t *testing.T) {
		cfg, configPath, databasePath := enabledCarpoolCommandConfig(t)
		terminalInput := matchingTerminal("empty-bootstrap-password")
		handled, errRun := runCarpoolCommand(context.Background(), cfg, configPath, carpoolCommandOptions{
			bootstrapSet: true,
		}, commandDependencies(databasePath, terminalInput, io.Discard))
		if !handled || errRun == nil {
			t.Fatalf("runCarpoolCommand() = (%t, %v), want validation error", handled, errRun)
		}
		if terminalInput.reads != 0 {
			t.Fatalf("ReadPassword() calls = %d, want 0", terminalInput.reads)
		}
		if _, errStat := os.Stat(databasePath); !errors.Is(errStat, os.ErrNotExist) {
			t.Fatalf("database stat error = %v, want not exist", errStat)
		}
	})

	t.Run("active database", func(t *testing.T) {
		cfg, configPath, databasePath := enabledCarpoolCommandConfig(t)
		if errMkdir := os.MkdirAll(filepath.Dir(databasePath), 0o700); errMkdir != nil {
			t.Fatalf("create database directory: %v", errMkdir)
		}
		if errWrite := os.WriteFile(databasePath+"-wal", []byte("active"), 0o600); errWrite != nil {
			t.Fatalf("write SQLite sidecar: %v", errWrite)
		}
		terminalInput := matchingTerminal("active-database-password")
		handled, errRun := runCarpoolCommand(context.Background(), cfg, configPath, carpoolCommandOptions{
			bootstrapAdmin: "admin",
		}, commandDependencies(databasePath, terminalInput, io.Discard))
		if !handled || errRun == nil || !strings.Contains(strings.ToLower(errRun.Error()), "sidecar") {
			t.Fatalf("runCarpoolCommand() = (%t, %v), want stopped database error", handled, errRun)
		}
		if terminalInput.reads != 0 {
			t.Fatalf("ReadPassword() calls = %d, want 0", terminalInput.reads)
		}
		if _, errStat := os.Stat(databasePath); !errors.Is(errStat, os.ErrNotExist) {
			t.Fatalf("database stat error = %v, want not exist", errStat)
		}
	})
}

func TestRunCarpoolBackupAndRestore(t *testing.T) {
	cfg, configPath, databasePath := enabledCarpoolCommandConfig(t)
	backupPath := filepath.Join(t.TempDir(), "backup.db")
	store := openCommandDatabase(t, databasePath)
	sourceCar, errCreate := store.CreateCar(context.Background(), domain.Car{Name: "source marker", Status: domain.CarStatusActive})
	if errCreate != nil {
		closeCommandDatabase(t, store)
		t.Fatalf("CreateCar() error = %v", errCreate)
	}
	closeCommandDatabase(t, store)

	var backupOutput bytes.Buffer
	handled, errBackup := runCarpoolCommand(context.Background(), cfg, configPath,
		carpoolCommandOptions{backupPath: backupPath}, commandDependencies(databasePath, nil, &backupOutput))
	if !handled || errBackup != nil {
		t.Fatalf("backup runCarpoolCommand() = (%t, %v)", handled, errBackup)
	}
	if _, errStat := os.Stat(ops.ManifestPath(backupPath)); errStat != nil {
		t.Fatalf("backup manifest: %v", errStat)
	}

	store = openCommandDatabase(t, databasePath)
	laterCar, errCreate := store.CreateCar(context.Background(), domain.Car{Name: "later marker", Status: domain.CarStatusActive})
	if errCreate != nil {
		closeCommandDatabase(t, store)
		t.Fatalf("CreateCar() error = %v", errCreate)
	}
	closeCommandDatabase(t, store)

	var restoreOutput bytes.Buffer
	handled, errRestore := runCarpoolCommand(context.Background(), cfg, configPath,
		carpoolCommandOptions{restorePath: backupPath}, commandDependencies(databasePath, nil, &restoreOutput))
	if !handled || errRestore != nil {
		t.Fatalf("restore runCarpoolCommand() = (%t, %v)", handled, errRestore)
	}
	restored := openCommandDatabase(t, databasePath)
	defer closeCommandDatabase(t, restored)
	if _, errGet := restored.GetCar(context.Background(), sourceCar.ID); errGet != nil {
		t.Fatalf("restored source car: %v", errGet)
	}
	if _, errGet := restored.GetCar(context.Background(), laterCar.ID); !errors.Is(errGet, domain.ErrNotFound) {
		t.Fatalf("later car GetCar() error = %v, want ErrNotFound", errGet)
	}
}

func TestCarpoolCommandRequestedInArgs(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{name: "none", args: []string{"--config", "config.yaml"}, want: false},
		{name: "bootstrap separate", args: []string{"--carpool-bootstrap-admin", "admin"}, want: true},
		{name: "random bootstrap", args: []string{"--carpool-bootstrap-random-admin=admin"}, want: true},
		{name: "backup equals", args: []string{"-carpool-backup=backup.db"}, want: true},
		{name: "restore double dash equals", args: []string{"--carpool-restore=backup.db"}, want: true},
		{name: "similar plugin flag", args: []string{"--plugin-carpool-backup=value"}, want: false},
		{name: "positional after terminator", args: []string{"--", "--carpool-backup=backup.db"}, want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := carpoolCommandRequestedInArgs(test.args); got != test.want {
				t.Fatalf("carpoolCommandRequestedInArgs(%v) = %t, want %t", test.args, got, test.want)
			}
		})
	}
}

func matchingTerminal(password string) *fakeCarpoolTerminal {
	return &fakeCarpoolTerminal{terminal: true, passwords: [][]byte{[]byte(password), []byte(password)}}
}

func fastPasswordHasher() carpoolservice.PasswordHasher {
	return carpoolservice.PasswordHasher{Params: carpoolservice.PasswordParams{
		Memory: 8, Iterations: 1, Parallelism: 1, SaltLength: 16, KeyLength: 16,
	}}
}

func enabledCarpoolCommandConfig(t *testing.T) (*config.Config, string, string) {
	t.Helper()
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	cfg := &config.Config{Carpool: config.DefaultCarpoolConfig()}
	cfg.Carpool.Enabled = true
	cfg.Carpool.DatabasePath = "./data/carpool.db"
	databasePath := filepath.Join(directory, "data", "carpool.db")
	return cfg, configPath, databasePath
}

func commandDependencies(databasePath string, terminalInput carpoolPasswordTerminal, output io.Writer) carpoolCommandDependencies {
	return carpoolCommandDependencies{
		terminal:       terminalInput,
		output:         output,
		passwordHasher: fastPasswordHasher(),
		resolveDatabasePath: func(config.CarpoolConfig, string) (string, error) {
			return databasePath, nil
		},
	}
}

func openCommandDatabase(t *testing.T, path string) *carpoolsqlite.Store {
	t.Helper()
	store, errOpen := carpoolsqlite.Open(context.Background(), carpoolsqlite.Config{Path: path})
	if errOpen != nil {
		t.Fatalf("sqlite.Open() error = %v", errOpen)
	}
	return store
}

func closeCommandDatabase(t *testing.T, store *carpoolsqlite.Store) {
	t.Helper()
	if errClose := store.Close(); errClose != nil {
		t.Fatalf("Store.Close() error = %v", errClose)
	}
}
