package models

import (
	"encoding/json"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
)

func TestAstraDefaultLowAcrossCatalogRefreshAndThinkingOverrides(t *testing.T) {
	original, revision := registry.GetCodexClientModelsSnapshot()
	t.Cleanup(func() {
		codexClientModelTemplatesMu.Lock()
		codexClientModelTemplatesLoaded = false
		codexClientModelTemplatesMu.Unlock()
		if _, _, err := loadCodexClientModelTemplatesSnapshot(original, revision); err != nil {
			t.Fatal(err)
		}
	})
	for _, source := range []string{"embedded", "refreshed-medium"} {
		t.Run(source, func(t *testing.T) {
			raw := original
			if source == "refreshed-medium" {
				var catalog codexClientModelsPayload
				if err := json.Unmarshal(original, &catalog); err != nil {
					t.Fatal(err)
				}
				for _, model := range catalog.Models {
					if model["slug"] == "gpt-6-astra" {
						model["default_reasoning_level"] = "medium"
					}
				}
				var err error
				raw, err = json.Marshal(catalog)
				if err != nil {
					t.Fatal(err)
				}
			}
			codexClientModelTemplatesMu.Lock()
			codexClientModelTemplatesLoaded = false
			codexClientModelTemplatesMu.Unlock()
			if _, _, err := loadCodexClientModelTemplatesSnapshot(raw, revision); err != nil {
				t.Fatal(err)
			}
			reg := registry.GetGlobalRegistry()
			reg.RegisterClient(t.Name(), "codex", []*registry.ModelInfo{
				{ID: "gpt-6-astra", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}},
				{ID: "custom-astra", MetadataModelID: "gpt-6-astra", Thinking: &registry.ThinkingSupport{Levels: []string{"medium", "low", "high"}}},
				{ID: "astra-no-low", MetadataModelID: "gpt-6-astra", ExplicitThinking: true, Thinking: &registry.ThinkingSupport{Levels: []string{"high"}}},
				{ID: "gpt-6-sol", Thinking: &registry.ThinkingSupport{Levels: []string{"low", "medium", "high"}}},
			})
			defer reg.UnregisterClient(t.Name())
			response := BuildResponseForClient(reg.GetAvailableModels("openai"), reg.GetModelProviders, false, "0.156.1")
			want := map[string]string{"gpt-6-astra": "low", "custom-astra": "low", "astra-no-low": "high", "gpt-6-sol": "medium"}
			seen := make(map[string]bool)
			for _, model := range response["models"].([]map[string]any) {
				id := stringModelValue(model, "slug")
				if expected, ok := want[id]; ok {
					seen[id] = true
					if got := stringModelValue(model, "default_reasoning_level"); got != expected {
						t.Errorf("%s default=%q want %q", id, got, expected)
					}
					if id == "gpt-6-sol" && model["node_repl_auto_review_required"] != false {
						t.Error("B4 changed")
					}
				}
			}
			for id := range want {
				if !seen[id] {
					t.Errorf("missing model %s", id)
				}
			}
		})
	}
}
