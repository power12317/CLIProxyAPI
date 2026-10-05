package cliproxy

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

// withPrismModels makes the adapter catalog routable even when the native Codex
// catalog has not learned a new Prism model yet. Account entitlement is decided
// by Prism, not by the native Codex plan catalog.
func withPrismModels(cfg *config.Config, auth *coreauth.Auth, models []*ModelInfo) []*ModelInfo {
	if !auth.PrismEligible(cfg) {
		return models
	}
	seen := make(map[string]bool, len(models))
	for _, model := range models {
		if model != nil {
			seen[model.ID] = true
		}
	}
	for _, id := range registry.PrismModelIDs() {
		if seen[id] {
			continue
		}
		models = append(models, &ModelInfo{
			ID: id, Object: "model", OwnedBy: "openai", Type: "codex", DisplayName: id,
			SupportedInputModalities:  []string{"text"},
			SupportedOutputModalities: []string{"text"},
			Thinking: &registry.ThinkingSupport{
				Levels: []string{"low", "medium", "high", "xhigh"},
			},
		})
	}
	return models
}
