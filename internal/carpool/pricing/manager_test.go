package pricing

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type memoryCatalogPersistence struct {
	record  CatalogRecord
	loaded  bool
	saved   int
	loadErr error
}

func (p *memoryCatalogPersistence) LoadPricingCatalog(context.Context) (CatalogRecord, error) {
	if p.loadErr != nil {
		return CatalogRecord{}, p.loadErr
	}
	if !p.loaded {
		return CatalogRecord{}, errors.New("not cached")
	}
	return p.record, nil
}

func (p *memoryCatalogPersistence) SavePricingCatalog(_ context.Context, record CatalogRecord) error {
	p.record = record
	p.loaded = true
	p.saved++
	return nil
}

func testCatalog(t *testing.T, model string) *Catalog {
	t.Helper()
	catalog, err := ParseCatalog([]byte(`{"`+model+`":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	return catalog
}

func TestManagerRefreshSwapsAndPersistsValidatedCatalog(t *testing.T) {
	initial := testCatalog(t, "initial")
	updated := `{"updated":{"input_cost_per_token":0.000003,"output_cost_per_token":0.000004}}`
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Fatalf("method = %s, want GET", r.Method)
		}
		_, _ = w.Write([]byte(updated))
	}))
	defer server.Close()

	persistence := &memoryCatalogPersistence{}
	now := time.Date(2026, 9, 5, 17, 0, 0, 0, time.UTC)
	manager, err := NewManager(context.Background(), ManagerConfig{
		Initial: initial, URL: server.URL + "?token=secret", Persistence: persistence,
		Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := manager.Refresh(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Current().Lookup("updated"); !ok {
		t.Fatal("refresh did not activate updated catalog")
	}
	if persistence.saved != 1 {
		t.Fatalf("saved = %d, want 1", persistence.saved)
	}
	status := manager.Status()
	if status.Hash != manager.Current().Hash || status.LastSuccessAt != now {
		t.Fatalf("status = %+v, current hash = %s", status, manager.Current().Hash)
	}
	if strings.Contains(status.CatalogURL, "secret") || strings.Contains(status.CatalogURL, "?") {
		t.Fatalf("status exposed URL query: %q", status.CatalogURL)
	}
}

func TestManagerRefreshKeepsPreviousCatalogOnFailure(t *testing.T) {
	initial := testCatalog(t, "initial")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer server.Close()

	now := time.Date(2026, 9, 5, 17, 0, 0, 0, time.UTC)
	manager, err := NewManager(context.Background(), ManagerConfig{Initial: initial, URL: server.URL, Now: func() time.Time { return now }})
	if err != nil {
		t.Fatal(err)
	}
	oldHash := manager.Current().Hash
	if err := manager.Refresh(context.Background()); err == nil {
		t.Fatal("Refresh() succeeded for a bad gateway")
	}
	if manager.Current().Hash != oldHash {
		t.Fatalf("failed refresh changed catalog hash to %s", manager.Current().Hash)
	}
	status := manager.Status()
	if status.LastCheckedAt != now || status.FailureReason == "" {
		t.Fatalf("failure status = %+v", status)
	}
}

func TestManagerLoadsCachedSnapshot(t *testing.T) {
	initial := testCatalog(t, "initial")
	cached := testCatalog(t, "cached")
	loadedAt := time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC)
	persistence := &memoryCatalogPersistence{loaded: true, record: CatalogRecord{Hash: cached.Hash, Source: cached.Source, Contents: cached.Raw, LoadedAt: loadedAt}}
	manager, err := NewManager(context.Background(), ManagerConfig{Initial: initial, Persistence: persistence})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := manager.Current().Lookup("cached"); !ok {
		t.Fatal("cached catalog was not loaded")
	}
	if got := manager.Status().LoadedAt; !got.Equal(loadedAt) {
		t.Fatalf("LoadedAt = %s, want %s", got, loadedAt)
	}
}
