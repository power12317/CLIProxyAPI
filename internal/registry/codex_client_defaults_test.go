package registry

import (
	"testing"

	"github.com/MoeForge/moego/ojson"
)

func TestCodexForkDefaultsSurviveCatalogRefresh(t *testing.T) {
	original, _ := GetCodexClientModelsSnapshot()
	t.Cleanup(func() {
		if _, err := loadCodexClientModelsFromBytes(original, "cleanup"); err != nil {
			t.Fatal(err)
		}
	})
	models := []map[string]any{
		testCodexClientModel("gpt-6-astra", 1),
		testCodexClientModel("gpt-5.5", 2),
		testCodexClientModel("unrelated-model", 3),
	}
	for _, model := range models {
		model["supported_reasoning_levels"] = []any{map[string]any{"effort": "low", "description": "Low"}, map[string]any{"effort": "medium", "description": "Medium"}}
		model["default_reasoning_level"] = "medium"
		model["node_repl_auto_review_required"] = false
	}
	remote := testCodexClientCatalog(t, models...)
	if _, err := loadCodexClientModelsFromBytes(remote, "remote"); err != nil {
		t.Fatal(err)
	}
	first, revision := GetCodexClientModelsSnapshot()
	for _, model := range ojson.GetBytes(first, "models").Array() {
		want := "low"
		if model.Get("slug").String() == "unrelated-model" {
			want = "medium"
		}
		if model.Get("default_reasoning_level").String() != want {
			t.Fatalf("unexpected default: %s", model.Raw)
		}
		if model.Get("node_repl_auto_review_required").Bool() || len(model.Get("supported_reasoning_levels").Array()) != 2 {
			t.Fatalf("refresh changed official capability/review metadata: %s", model.Raw)
		}
	}
	if changed, err := loadCodexClientModelsFromBytes(remote, "repeat remote"); err != nil || changed {
		t.Fatalf("identical refresh changed catalog: changed=%v err=%v", changed, err)
	}
	if got := GetCodexClientModelsRevision(); got != revision {
		t.Fatalf("identical refresh advanced revision: %d != %d", got, revision)
	}
}
