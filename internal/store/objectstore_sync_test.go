package store

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/minio/minio-go/v7"
)

func newObjectSyncTestStore(t *testing.T, transport http.RoundTripper) *ObjectTokenStore {
	t.Helper()
	client, errNew := minio.New("unused.invalid", &minio.Options{
		Secure:       true,
		Region:       "us-east-1",
		BucketLookup: minio.BucketLookupPath,
		Transport:    transport,
	})
	if errNew != nil {
		t.Fatal(errNew)
	}
	return &ObjectTokenStore{
		client:  client,
		cfg:     ObjectStoreConfig{Bucket: "test-bucket", Prefix: "tenant"},
		authDir: t.TempDir(),
	}
}

func objectSyncTestResponse(r *http.Request, status int, body string) *http.Response {
	header := make(http.Header)
	header.Set("Content-Length", strconv.Itoa(len(body)))
	header.Set("Last-Modified", "Mon, 01 Jan 2024 00:00:00 GMT")
	return &http.Response{
		StatusCode:    status,
		Header:        header,
		Body:          io.NopCloser(strings.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       r,
	}
}

// All keys used by these tests are synthetic and need no XML escaping.
func objectSyncTestListing(next string, keys ...string) string {
	var body strings.Builder
	fmt.Fprintf(&body, `<ListBucketResult xmlns="http://s3.amazonaws.com/doc/2006-03-01/"><Name>test-bucket</Name><Prefix>tenant/auths/</Prefix><IsTruncated>%t</IsTruncated><NextContinuationToken>%s</NextContinuationToken>`, next != "", next)
	for _, key := range keys {
		fmt.Fprintf(&body, `<Contents><Key>%s</Key><LastModified>2024-01-01T00:00:00Z</LastModified><Size>16</Size></Contents>`, key)
	}
	body.WriteString("</ListBucketResult>")
	return body.String()
}

func writeObjectSyncTestFile(t *testing.T, store *ObjectTokenStore, name, content string) string {
	t.Helper()
	path := filepath.Join(store.authDir, filepath.FromSlash(name))
	if errMkdir := os.MkdirAll(filepath.Dir(path), 0o700); errMkdir != nil {
		t.Fatal(errMkdir)
	}
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatal(errWrite)
	}
	return path
}

func TestObjectSyncRemovesStaleMirrorsWithoutRemoteDeletion(t *testing.T) {
	for _, withLiveObjects := range []bool{false, true} {
		t.Run(fmt.Sprintf("live-objects=%t", withLiveObjects), func(t *testing.T) {
			var mu sync.Mutex
			var calls []string
			store := newObjectSyncTestStore(t, strictSaveTransport(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				calls = append(calls, r.Method+" "+r.URL.Path)
				mu.Unlock()
				if r.Method != http.MethodGet {
					return nil, fmt.Errorf("unexpected remote mutation: %s", r.Method)
				}
				if r.URL.Query().Get("list-type") == "2" {
					if got := r.URL.Query().Get("prefix"); got != "tenant/auths/" {
						return nil, fmt.Errorf("unexpected prefix: %q", got)
					}
					next := r.URL.Query().Get("continuation-token")
					body := objectSyncTestListing("")
					if withLiveObjects {
						if next == "" {
							body = objectSyncTestListing("next-page", "tenant/auths/live.json")
						} else if next == "next-page" {
							body = objectSyncTestListing("", "tenant/auths/nested/live.json")
						} else {
							return nil, fmt.Errorf("unexpected continuation token: %q", next)
						}
					}
					return objectSyncTestResponse(r, http.StatusOK, body), nil
				}
				if r.URL.Path != "/test-bucket/tenant/auths/live.json" && r.URL.Path != "/test-bucket/tenant/auths/nested/live.json" {
					return nil, fmt.Errorf("unexpected object: %s", r.URL.Path)
				}
				return objectSyncTestResponse(r, http.StatusOK, `{"type":"codex"}`), nil
			}))
			stale := []string{
				writeObjectSyncTestFile(t, store, "deleted.json", `{"type":"codex","access_token":"synthetic-deleted"}`),
				writeObjectSyncTestFile(t, store, "nested/deleted.JSON", `{"type":"codex"}`),
			}
			note := writeObjectSyncTestFile(t, store, "nested/note.txt", "keep local non-auth data")
			if withLiveObjects {
				writeObjectSyncTestFile(t, store, "live.json", `{"type":"codex","label":"old"}`)
			}
			if errSync := store.syncAuthFromBucket(context.Background()); errSync != nil {
				t.Fatal(errSync)
			}
			for _, path := range stale {
				if _, errStat := os.Stat(path); !errors.Is(errStat, os.ErrNotExist) {
					t.Fatalf("stale auth mirror still present: %s: %v", path, errStat)
				}
			}
			if data, errRead := os.ReadFile(note); errRead != nil || string(data) != "keep local non-auth data" {
				t.Fatalf("non-auth file modified: %q, %v", data, errRead)
			}
			auths, errList := store.List(context.Background())
			if errList != nil {
				t.Fatal(errList)
			}
			wantAuths, wantCalls := 0, 1
			if withLiveObjects {
				wantAuths, wantCalls = 2, 4
				for _, name := range []string{"live.json", "nested/live.json"} {
					if data, errRead := os.ReadFile(filepath.Join(store.authDir, filepath.FromSlash(name))); errRead != nil || string(data) != `{"type":"codex"}` {
						t.Fatalf("live mirror not downloaded: %s: %q, %v", name, data, errRead)
					}
				}
			}
			if len(auths) != wantAuths {
				t.Fatalf("List returned %d auths, want %d", len(auths), wantAuths)
			}
			// Repeated watcher notifications, including relative paths, must not
			// delete objects that another instance may already have recreated.
			for range 2 {
				if errPersist := store.PersistAuthFiles(context.Background(), "Remove auth deleted.json", stale...); errPersist != nil {
					t.Fatal(errPersist)
				}
				if errPersist := store.PersistAuthFiles(context.Background(), "Remove auth deleted.json", "deleted.json"); errPersist != nil {
					t.Fatal(errPersist)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if len(calls) != wantCalls {
				t.Fatalf("unexpected remote requests after sync: %v", calls)
			}
		})
	}
}

func TestObjectSyncFailureDoesNotPruneMirrors(t *testing.T) {
	for _, failure := range []string{"first-page", "later-page", "download", "cancel"} {
		t.Run(failure, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var mu sync.Mutex
			pages := 0
			store := newObjectSyncTestStore(t, strictSaveTransport(func(r *http.Request) (*http.Response, error) {
				if r.Method != http.MethodGet {
					return nil, fmt.Errorf("unexpected remote mutation: %s", r.Method)
				}
				if r.URL.Query().Get("list-type") == "2" {
					mu.Lock()
					pages++
					mu.Unlock()
					if failure == "cancel" {
						cancel()
						return nil, ctx.Err()
					}
					if failure == "first-page" || r.URL.Query().Get("continuation-token") != "" {
						return objectSyncTestResponse(r, http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>synthetic failure</Message></Error>`), nil
					}
					next := ""
					if failure == "later-page" {
						next = "next-page"
					}
					return objectSyncTestResponse(r, http.StatusOK, objectSyncTestListing(next, "tenant/auths/live.json", "tenant/auths/later.json", "tenant/auths/last.json")), nil
				}
				if failure == "download" {
					return objectSyncTestResponse(r, http.StatusForbidden, `<Error><Code>AccessDenied</Code><Message>synthetic failure</Message></Error>`), nil
				}
				return objectSyncTestResponse(r, http.StatusOK, `{"type":"codex"}`), nil
			}))
			stale := writeObjectSyncTestFile(t, store, "stale.json", `{"type":"codex","label":"keep on failure"}`)
			errSync := store.syncAuthFromBucket(ctx)
			if errSync == nil {
				t.Fatal("incomplete sync succeeded")
			}
			if failure == "cancel" && !errors.Is(errSync, context.Canceled) {
				t.Fatalf("cancellation cause lost: %v", errSync)
			}
			if data, errRead := os.ReadFile(stale); errRead != nil || string(data) != `{"type":"codex","label":"keep on failure"}` {
				t.Fatalf("incomplete sync pruned a mirror: %q, %v", data, errRead)
			}
			if len(store.syncedAuthRemovals) != 0 {
				t.Fatalf("incomplete sync recorded removals: %v", store.syncedAuthRemovals)
			}
			mu.Lock()
			defer mu.Unlock()
			wantPages := 1
			if failure == "later-page" {
				wantPages = 2
			}
			if pages != wantPages {
				t.Fatalf("listing pages = %d, want %d", pages, wantPages)
			}
		})
	}
}

func TestObjectSyncRemovalSuppressionAllowsRecreationAndExplicitDelete(t *testing.T) {
	for _, recreation := range []string{"local", "remote", "explicit-delete"} {
		t.Run(recreation, func(t *testing.T) {
			var mu sync.Mutex
			remoteExists := false
			puts, deletes := 0, 0
			store := newObjectSyncTestStore(t, strictSaveTransport(func(r *http.Request) (*http.Response, error) {
				mu.Lock()
				defer mu.Unlock()
				switch r.Method {
				case http.MethodGet:
					if r.URL.Query().Get("list-type") == "2" {
						body := objectSyncTestListing("")
						if remoteExists {
							body = objectSyncTestListing("", "tenant/auths/again.json")
						}
						return objectSyncTestResponse(r, http.StatusOK, body), nil
					}
					return objectSyncTestResponse(r, http.StatusOK, `{"type":"codex"}`), nil
				case http.MethodPut:
					puts++
					return objectSyncTestResponse(r, http.StatusOK, ""), nil
				case http.MethodDelete:
					deletes++
					return objectSyncTestResponse(r, http.StatusNoContent, ""), nil
				default:
					return nil, fmt.Errorf("unexpected method: %s", r.Method)
				}
			}))
			path := writeObjectSyncTestFile(t, store, "again.json", `{"type":"codex"}`)
			if errSync := store.syncAuthFromBucket(context.Background()); errSync != nil {
				t.Fatal(errSync)
			}
			if errPersist := store.PersistAuthFiles(context.Background(), "Remove auth again.json", path); errPersist != nil {
				t.Fatal(errPersist)
			}
			mu.Lock()
			initialDeletes := deletes
			mu.Unlock()
			if initialDeletes != 0 {
				t.Fatal("reconciliation propagated a remote deletion")
			}
			switch recreation {
			case "local":
				writeObjectSyncTestFile(t, store, "again.json", `{"type":"codex"}`)
				if errPersist := store.PersistAuthFiles(context.Background(), "Sync auth again.json", path); errPersist != nil {
					t.Fatal(errPersist)
				}
			case "remote":
				mu.Lock()
				remoteExists = true
				mu.Unlock()
				if errSync := store.syncAuthFromBucket(context.Background()); errSync != nil {
					t.Fatal(errSync)
				}
			}
			if recreation == "explicit-delete" {
				if errDelete := store.Delete(context.Background(), "again.json"); errDelete != nil {
					t.Fatal(errDelete)
				}
			} else {
				if errRemove := os.Remove(path); errRemove != nil {
					t.Fatal(errRemove)
				}
				if errPersist := store.PersistAuthFiles(context.Background(), "Remove auth again.json", path); errPersist != nil {
					t.Fatal(errPersist)
				}
			}
			mu.Lock()
			defer mu.Unlock()
			wantPuts := 0
			if recreation == "local" {
				wantPuts = 1
			}
			if deletes != 1 || puts != wantPuts {
				t.Fatalf("remote mutations after recreation: deletes=%d puts=%d", deletes, puts)
			}
		})
	}
}
