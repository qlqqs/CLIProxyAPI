package pricing

import (
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	"testing"
)

func TestParseCatalogAndCalculateExact(t *testing.T) {
	c, err := ParseCatalog([]byte(`{"demo":{"input_cost_per_token":"0.0000000015","output_cost_per_token":"0.0000000025","cache_read_input_token_cost":"0.0000000005"}}`), "test")
	if err != nil {
		t.Fatal(err)
	}
	p, ok := c.Lookup("DEMO")
	if !ok {
		t.Fatal("missing")
	}
	b := usage.NewIndependentTokenBreakdown(1, 2, 0, 3, 0, 6)
	got := Calculate(p, b, "default")
	if !got.Known || got.NanoUSD != 10 {
		t.Fatalf("cost=%+v", got)
	}
}
func TestMissingDimensionUnknown(t *testing.T) {
	c, _ := ParseCatalog([]byte(`{"demo":{"input_cost_per_token":0.1,"output_cost_per_token":0.2}}`), "")
	p, _ := c.Lookup("demo")
	b := usage.NewIndependentTokenBreakdown(0, 1, 0, 0, 0, 1)
	if got := Calculate(p, b, ""); got.Known {
		t.Fatal("expected unknown")
	}
}
