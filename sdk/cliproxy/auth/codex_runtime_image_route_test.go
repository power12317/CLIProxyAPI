package auth

import (
	"context"
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
)

func TestRuntimeImageRotationExcludesCPAOwnedCredentials(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			m := NewManager(nil, &FillFirstSelector{}, nil)
			m.SetConfig(&config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}}})
			exec := &retryRoundCallExecutor{identifier: "codex"}
			m.RegisterExecutor(exec)
			model := "image-route-rotation"
			for _, id := range []string{"a-runtime", "b-local", "c-runtime"} {
				registry.GetGlobalRegistry().RegisterClient(id, "codex", []*registry.ModelInfo{{ID: model}})
				t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(id) })
				_, err := m.Register(context.Background(), &Auth{ID: id, Provider: "codex", Metadata: map[string]any{"disable_cooling": true, "codex_cli": map[string]any{"enabled": id != "b-local"}}})
				if err != nil {
					t.Fatal(err)
				}
			}
			req := core.Request{Model: model, Payload: []byte(`{"prompt":"draw"}`)}
			opts := core.Options{SourceFormat: translator.FromString("openai-image"), Metadata: map[string]any{core.RequestPathMetadataKey: "/v1/images/generations"}}
			kind := "execute"
			if stream {
				kind = "stream"
				_, _ = m.ExecuteStream(context.Background(), []string{"codex"}, req, opts)
			} else {
				_, _ = m.Execute(context.Background(), []string{"codex"}, req, opts)
			}
			ids := exec.ids(kind)
			if len(ids) != 2 || ids[0] != "a-runtime" || ids[1] != "c-runtime" {
				t.Fatalf("image retry selected local credentials or failed to rotate: %v", ids)
			}
		})
	}
}
