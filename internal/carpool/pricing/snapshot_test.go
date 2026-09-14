package pricing

import (
	"context"
	"testing"
)

func TestSnapshotOwnsPricesIncludingServiceTiers(t *testing.T) {
	raw := []byte(`{"model":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002,"input_cost_per_token_priority":0.000003,"output_cost_per_token_priority":0.000004}}`)
	catalog, err := ParseCatalog(raw, "test")
	if err != nil {
		t.Fatal(err)
	}
	frozen := Freeze(catalog)
	manager, err := NewManager(context.Background(), ManagerConfig{Initial: catalog})
	if err != nil {
		t.Fatal(err)
	}
	shared := manager.Snapshot()
	catalog.Models["model"].InputPerToken.SetInt64(99)
	catalog.Models["model"].Tiers["priority"].InputPerToken.SetInt64(99)
	exposed := manager.Current()
	exposed.Models["model"].InputPerToken.SetInt64(88)
	delete(exposed.Models, "model")
	for _, snapshot := range []*Snapshot{frozen, shared, manager.Snapshot()} {
		price, ok := snapshot.Lookup("model")
		if !ok || price.InputPerToken.FloatString(6) != "0.000001" {
			t.Fatal("snapshot aliases caller catalog")
		}
		tier, ok := price.Tiers["priority"]
		if !ok || tier.InputPerToken.FloatString(6) != "0.000003" {
			t.Fatalf("tier clone missing or mutated: %+v", price)
		}
		tier.InputPerToken.SetInt64(44)
		delete(price.Tiers, "priority")
		again, _ := snapshot.Lookup("model")
		if again.Tiers["priority"].InputPerToken.FloatString(6) != "0.000003" {
			t.Fatal("lookup leaked mutable tier")
		}
	}
}
