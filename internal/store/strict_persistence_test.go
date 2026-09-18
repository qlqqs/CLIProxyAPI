package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/minio/minio-go/v7"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

type strictSaveTransport func(*http.Request) (*http.Response, error)

func (f strictSaveTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type strictSaveConnector struct {
	exec func(context.Context, string, []driver.NamedValue) (driver.Result, error)
}

func (c *strictSaveConnector) Connect(context.Context) (driver.Conn, error) {
	return &strictSaveConn{c}, nil
}
func (c *strictSaveConnector) Driver() driver.Driver { return strictSaveDriver{} }

type strictSaveDriver struct{}

func (strictSaveDriver) Open(string) (driver.Conn, error) { return nil, errors.New("use connector") }

type strictSaveConn struct{ *strictSaveConnector }

func (*strictSaveConn) Prepare(string) (driver.Stmt, error) {
	return nil, errors.New("unexpected prepare")
}
func (*strictSaveConn) Close() error              { return nil }
func (*strictSaveConn) Begin() (driver.Tx, error) { return nil, errors.New("unexpected transaction") }
func (c *strictSaveConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	return c.exec(ctx, query, args)
}

func TestRemoteStoreSaveStrictPersistenceRetry(t *testing.T) {
	for _, backend := range []string{"object", "postgres"} {
		for _, strict := range []bool{false, true} {
			name := "default"
			if strict {
				name = "strict"
			}
			for _, missingMirror := range []bool{false, true} {
				mirrorName := "existing-mirror"
				if missingMirror {
					mirrorName = "missing-mirror"
				}
				t.Run(backend+"/"+name+"/"+mirrorName, func(t *testing.T) {
					dir := t.TempDir()
					path := filepath.Join(dir, "retry.json")
					if !missingMirror {
						if err := os.WriteFile(path, []byte(`{"disabled":false,"type":"test"}`), 0o600); err != nil {
							t.Fatal(err)
						}
					}
					calls := 0
					fail := true
					originalRemote := []byte(`{"disabled":false,"type":"test"}`)
					durable := append([]byte(nil), originalRemote...)
					persist := func(data []byte) error {
						calls++
						if fail {
							return errors.New("remote rejected write")
						}
						durable = append([]byte(nil), data...)
						return nil
					}
					var store coreauth.Store
					if backend == "object" {
						client, err := minio.New("unused.invalid", &minio.Options{
							Secure: true, Region: "us-east-1", BucketLookup: minio.BucketLookupPath,
							Transport: strictSaveTransport(func(r *http.Request) (*http.Response, error) {
								if r.Method != http.MethodPut {
									t.Fatalf("unexpected method %s", r.Method)
								}
								data, errRead := io.ReadAll(r.Body)
								if errRead != nil {
									return nil, errRead
								}
								status, body := http.StatusOK, ""
								if persist(data) != nil {
									status, body = http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>rejected</Message></Error>`
								}
								return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
							}),
						})
						if err != nil {
							t.Fatal(err)
						}
						store = &ObjectTokenStore{client: client, cfg: ObjectStoreConfig{Bucket: "test-bucket"}, authDir: dir}
					} else {
						db := sql.OpenDB(&strictSaveConnector{exec: func(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
							if coreauth.StrictPersistenceRequired(ctx) != strict {
								t.Fatal("strict context not propagated")
							}
							if !strings.Contains(query, "INSERT INTO") || len(args) != 2 || args[0].Value != "retry.json" {
								t.Fatalf("unexpected upsert: %s %#v", query, args)
							}
							data, ok := args[1].Value.([]byte)
							if !ok {
								t.Fatalf("unexpected payload type %T", args[1].Value)
							}
							if err := persist(data); err != nil {
								return nil, err
							}
							return driver.RowsAffected(1), nil
						}})
						t.Cleanup(func() {
							if err := db.Close(); err != nil {
								t.Error(err)
							}
						})
						store = &PostgresStore{db: db, cfg: PostgresStoreConfig{AuthTable: "auth_store"}, authDir: dir}
					}
					ctx := context.Background()
					if strict {
						ctx = coreauth.WithStrictPersistence(ctx)
					}
					auth := &coreauth.Auth{ID: "retry.json", FileName: "retry.json", Disabled: true, Metadata: map[string]any{"type": "test"}}
					if missingMirror && !strict {
						saved, err := store.Save(ctx, auth)
						if err != nil || saved != "" || calls != 0 || !jsonEqual(durable, originalRemote) {
							t.Fatalf("default missing mirror: path=%q calls=%d err=%v", saved, calls, err)
						}
						if _, errStat := os.Stat(path); !errors.Is(errStat, os.ErrNotExist) {
							t.Fatalf("default created mirror: %v", errStat)
						}
						return
					}
					if _, err := store.Save(ctx, auth); err == nil {
						t.Fatal("initial remote failure returned success")
					}
					mirror, err := os.ReadFile(path)
					if err != nil {
						t.Fatal(err)
					}
					if !jsonEqual(mirror, []byte(`{"disabled":true,"type":"test"}`)) {
						t.Fatalf("mirror not updated: %s", mirror)
					}
					if calls != 1 || !jsonEqual(durable, originalRemote) {
						t.Fatalf("initial calls=%d durable=%s", calls, durable)
					}
					_, err = store.Save(ctx, auth)
					if strict {
						if err == nil || calls != 2 {
							t.Fatalf("identical failed retry: calls=%d err=%v", calls, err)
						}
					} else if err != nil || calls != 1 {
						t.Fatalf("default behavior changed: calls=%d err=%v", calls, err)
					}
					fail = false
					saved, err := store.Save(ctx, auth)
					if err != nil || saved != path {
						t.Fatalf("successful retry: path=%q err=%v", saved, err)
					}
					if strict {
						if calls != 3 || !jsonEqual(durable, mirror) {
							t.Fatalf("durable retry missing: calls=%d durable=%s", calls, durable)
						}
					} else if calls != 1 || !jsonEqual(durable, originalRemote) {
						t.Fatalf("default retry contacted remote: calls=%d", calls)
					}
				})
			}
		}
	}
}
