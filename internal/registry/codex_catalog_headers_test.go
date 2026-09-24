package registry

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCodexCatalogHeaderNormalizationOnLoadAndRefresh(t *testing.T) {
	const oldUA = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	data := map[string]any{}
	for _, section := range []string{"codex-free", "codex-team", "codex-plus", "codex-pro", "xai"} {
		data[section] = []*ModelInfo{
			{ID: "gpt-5.6-luna", Config: &ModelConfig{OverrideHeader: map[string]string{"User-Agent": oldUA, "originator": "codex-tui", "X-Custom": "keep"}}},
			{ID: "custom-model", Config: &ModelConfig{OverrideHeader: map[string]string{"User-Agent": oldUA}}},
		}
	}
	// A deliberate replacement UA in a catalog must also survive.
	data["codex-team"].([]*ModelInfo)[0].Config.OverrideHeader["User-Agent"] = "custom-catalog-ua"
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatal(err)
	}
	oldStore, oldURLs := getModels(), modelsURLs
	t.Cleanup(func() {
		modelsCatalogStore.mu.Lock()
		modelsCatalogStore.data = oldStore
		modelsCatalogStore.mu.Unlock()
		modelsURLs = oldURLs
	})
	check := func(t *testing.T, catalog *staticModelsJSON) {
		t.Helper()
		for _, models := range [][]*ModelInfo{catalog.CodexFree, catalog.CodexPlus, catalog.CodexPro} {
			for key := range models[0].Config.OverrideHeader {
				if strings.EqualFold(key, "User-Agent") {
					t.Fatal("retired catalog UA was restored")
				}
			}
			if models[0].Config.OverrideHeader["originator"] != "codex-tui" || models[0].Config.OverrideHeader["X-Custom"] != "keep" {
				t.Fatal("unrelated header changed")
			}
			if models[1].Config.OverrideHeader["User-Agent"] != oldUA {
				t.Fatal("another model override changed")
			}
		}
		if catalog.CodexTeam[0].Config.OverrideHeader["User-Agent"] != "custom-catalog-ua" || catalog.XAI[0].Config.OverrideHeader["User-Agent"] != oldUA {
			t.Fatal("custom/provider override changed")
		}
	}
	t.Run("load", func(t *testing.T) {
		if err := loadModelsFromBytes(raw, "test"); err != nil {
			t.Fatal(err)
		}
		check(t, getModels())
	})
	t.Run("remote", func(t *testing.T) {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write(raw) }))
		defer server.Close()
		modelsURLs = []string{server.URL}
		catalog, source := fetchModelsFromRemote(context.Background())
		if catalog == nil || source != server.URL {
			t.Fatal("remote catalog was not loaded")
		}
		check(t, catalog)
	})
}
