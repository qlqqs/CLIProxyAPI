package registry

import (
	"reflect"
	"sort"
	"testing"
)

func TestGetAvailableModelsForClientsFiltersAndClones(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("scope-client-a", "openai", []*ModelInfo{
		{ID: "scope-model-a", DisplayName: "Model A", SupportedParameters: []string{"temperature"}},
		{ID: "scope-model-shared", DisplayName: "Shared A"},
	})
	r.RegisterClient("scope-client-b", "openai", []*ModelInfo{
		{ID: "scope-model-b", DisplayName: "Model B"},
		{ID: "scope-model-shared", DisplayName: "Shared B"},
	})
	r.RegisterClient("scope-client-outside", "openai", []*ModelInfo{{ID: "scope-model-outside"}})

	models := r.GetAvailableModelsForClients("openai", []string{" scope-client-b ", "scope-client-a", "scope-client-a", "missing"})
	if got, want := scopedModelIDs(models), []string{"scope-model-a", "scope-model-b", "scope-model-shared"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("GetAvailableModelsForClients() IDs = %#v, want %#v", got, want)
	}
	models[0]["id"] = "mutated"
	if params, ok := models[0]["supported_parameters"].([]string); ok && len(params) > 0 {
		params[0] = "mutated"
	}

	again := r.GetAvailableModelsForClients("openai", []string{"scope-client-a", "scope-client-b"})
	if got, want := scopedModelIDs(again), []string{"scope-model-a", "scope-model-b", "scope-model-shared"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("second GetAvailableModelsForClients() IDs = %#v, want %#v", got, want)
	}
	params, ok := again[0]["supported_parameters"].([]string)
	if !ok || len(params) != 1 || params[0] != "temperature" {
		t.Fatalf("second supported_parameters = %#v", again[0]["supported_parameters"])
	}

	infos := r.GetAvailableModelInfosForClients([]string{"scope-client-a"})
	if len(infos) != 2 {
		t.Fatalf("GetAvailableModelInfosForClients() length = %d, want 2", len(infos))
	}
	infos[0].DisplayName = "mutated"
	if next := r.GetAvailableModelInfosForClients([]string{"scope-client-a"}); next[0].DisplayName == "mutated" {
		t.Fatal("GetAvailableModelInfosForClients() returned mutable registry data")
	}

	if got := r.GetAvailableModelsForClients("openai", nil); got != nil {
		t.Fatalf("nil client scope returned %#v, want nil", got)
	}
	if got := r.GetAvailableModelInfosForClients([]string{" ", "missing"}); len(got) != 0 {
		t.Fatalf("unknown client scope returned %#v, want no models", got)
	}
}

func TestGetAvailableModelsForClientsHonorsPerClientSuspension(t *testing.T) {
	r := newTestModelRegistry()
	r.RegisterClient("scope-suspend-a", "openai", []*ModelInfo{
		{ID: "scope-only-a"},
		{ID: "scope-shared"},
	})
	r.RegisterClient("scope-suspend-b", "openai", []*ModelInfo{{ID: "scope-shared"}})

	r.SuspendClientModel("scope-suspend-a", "scope-only-a", "")
	if got := scopedModelIDs(r.GetAvailableModelsForClients("openai", []string{"scope-suspend-a"})); !reflect.DeepEqual(got, []string{"scope-shared"}) {
		t.Fatalf("empty-reason suspension returned IDs %#v", got)
	}

	r.SuspendClientModel("scope-suspend-a", "scope-shared", "maintenance")
	if got := scopedModelIDs(r.GetAvailableModelsForClients("openai", []string{"scope-suspend-a", "scope-suspend-b"})); !reflect.DeepEqual(got, []string{"scope-shared"}) {
		t.Fatalf("one available shared client returned IDs %#v", got)
	}
	r.SuspendClientModel("scope-suspend-b", "scope-shared", "maintenance")
	if got := r.GetAvailableModelsForClients("openai", []string{"scope-suspend-a", "scope-suspend-b"}); len(got) != 0 {
		t.Fatalf("all suspended clients returned %#v", got)
	}

	r.ResumeClientModel("scope-suspend-a", "scope-shared")
	r.SuspendClientModel("scope-suspend-a", "scope-shared", "quota")
	if got := scopedModelIDs(r.GetAvailableModelsForClients("openai", []string{"scope-suspend-a"})); !reflect.DeepEqual(got, []string{"scope-shared"}) {
		t.Fatalf("quota suspension returned IDs %#v", got)
	}
}

func scopedModelIDs(models []map[string]any) []string {
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if id, ok := model["id"].(string); ok {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}
