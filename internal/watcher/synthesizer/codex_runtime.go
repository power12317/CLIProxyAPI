package synthesizer

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/diff"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func (s *ConfigSynthesizer) synthesizeCodexRuntime(ctx *SynthesisContext) []*coreauth.Auth {
	if !ctx.Config.Codex.Runtime.Enabled {
		return nil
	}
	var out []*coreauth.Auth
	for _, worker := range ctx.Config.Codex.Runtime.Workers {
		models := make([]config.CodexModel, 0, len(worker.Models))
		for _, model := range worker.Models {
			models = append(models, config.CodexModel{Name: model, Alias: model})
		}
		status := coreauth.StatusActive
		if worker.Disabled {
			status = coreauth.StatusDisabled
		}
		out = append(out, &coreauth.Auth{
			ID: "codex-runtime:" + worker.ID, Provider: coreauth.CodexRuntimeProvider,
			Label: worker.ID, Prefix: worker.Prefix, Disabled: worker.Disabled, Status: status,
			CreatedAt: ctx.Now, UpdatedAt: ctx.Now,
			Attributes: map[string]string{
				coreauth.AttributeCodexRuntimeID: worker.ID,
				coreauth.AttributeAuthKind:       coreauth.AuthKindOAuth,
				coreauth.AttributeSource:         "config:codex-runtime",
				coreauth.AttributeRuntimeOnly:    "true",
				"account_id":                     worker.AccountID, "socket": worker.Socket,
				"models_hash": diff.ComputeCodexModelsHash(models),
			},
		})
	}
	return out
}
