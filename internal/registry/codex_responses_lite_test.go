package registry

import "testing"

func TestCodex156CatalogPreservesExplicitLiteCapability(t *testing.T) {
	const known = "test-cli-156-lite"
	defer func() {
		codexResponsesLiteModels.Lock()
		delete(codexResponsesLiteModels.values, known)
		codexResponsesLiteModels.Unlock()
	}()
	UpdateCodexResponsesLiteCapabilitiesFromCatalog([]byte(`{"models":[{"slug":"test-cli-156-lite","use_responses_lite":true,"new_optional":{"nested":1}}]}`))
	if !CodexModelUsesResponsesLite(known) {
		t.Fatal("explicit capability lost")
	}
	UpdateCodexResponsesLiteCapabilitiesFromCatalog([]byte(`{"models":[{"slug":"test-cli-156-lite","use_responses_lite":false}]}`))
	if CodexModelUsesResponsesLite(known) {
		t.Fatal("false ignored")
	}
	UpdateCodexResponsesLiteCapabilitiesFromCatalog([]byte(`{"models":[{"slug":"test-cli-156-lite"}]}`))
	if CodexModelUsesResponsesLite(known) || CodexModelUsesResponsesLite("test-unknown-cli-156") {
		t.Fatal("missing capability enabled Lite")
	}
}
