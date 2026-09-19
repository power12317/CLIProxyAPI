package registry

import (
	"encoding/json"
	"strings"
	"sync"
)

var codexResponsesLiteModels = struct {
	sync.RWMutex
	values map[string]bool
}{values: make(map[string]bool)}

// UpdateCodexResponsesLiteCapabilities replaces the cached capability values
// for the supplied Codex model IDs. A missing field is represented as false.
func UpdateCodexResponsesLiteCapabilities(values map[string]bool) {
	if len(values) == 0 {
		return
	}
	codexResponsesLiteModels.Lock()
	defer codexResponsesLiteModels.Unlock()
	for model, useLite := range values {
		model = normalizeCodexCapabilityModel(model)
		if model != "" {
			codexResponsesLiteModels.values[model] = useLite
		}
	}
}

// UpdateCodexResponsesLiteCapabilitiesFromCatalog records use_responses_lite
// values from a Codex client model catalog.
func UpdateCodexResponsesLiteCapabilitiesFromCatalog(data []byte) {
	var payload struct {
		Models []map[string]any `json:"models"`
	}
	if json.Unmarshal(data, &payload) != nil {
		return
	}
	values := make(map[string]bool, len(payload.Models))
	for _, model := range payload.Models {
		slug, _ := model["slug"].(string)
		if strings.TrimSpace(slug) == "" {
			continue
		}
		useLite, _ := model["use_responses_lite"].(bool)
		values[slug] = useLite
	}
	UpdateCodexResponsesLiteCapabilities(values)
}

// CodexModelUsesResponsesLite returns the current model capability. Unknown
// models intentionally return false, matching Codex CLI fallback behavior.
func CodexModelUsesResponsesLite(model string) bool {
	model = normalizeCodexCapabilityModel(model)
	if model == "" {
		return false
	}
	codexResponsesLiteModels.RLock()
	useLite, ok := codexResponsesLiteModels.values[model]
	codexResponsesLiteModels.RUnlock()
	if ok {
		return useLite
	}
	if info := LookupModelInfo(model, "codex"); info != nil {
		return info.UseResponsesLite
	}
	return false
}

func normalizeCodexCapabilityModel(model string) string {
	model = strings.TrimSpace(model)
	if index := strings.IndexByte(model, ':'); index > 0 {
		model = model[index+1:]
	}
	return strings.TrimSpace(model)
}
