package pricing

import (
	_ "embed"
	"fmt"
)

//go:embed data/model_prices_and_context_window.json
var defaultCatalogJSON []byte

// DefaultCatalog parses the bundled LiteLLM price snapshot.
func DefaultCatalog() (*Catalog, error) {
	catalog, err := ParseCatalog(defaultCatalogJSON, "bundled-litellm")
	if err != nil {
		return nil, fmt.Errorf("pricing catalog: load bundled snapshot: %w", err)
	}
	return catalog, nil
}
