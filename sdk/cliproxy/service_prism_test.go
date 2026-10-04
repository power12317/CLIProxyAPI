package cliproxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"testing"

	internalconfig "github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestPrismCatalogHotReloadAndExecution(t *testing.T) {
	t.Setenv("PRISM_ADAPTER_API_KEY", "synthetic-bridge-key")
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		if got := gjson.GetBytes(body, "model").String(); got != "gpt-6.1-sol" {
			t.Errorf("adapter model = %q", got)
		}
		if got := gjson.GetBytes(body, "reasoning.effort").String(); got != "high" {
			t.Errorf("adapter effort = %q", got)
		}
		response := map[string]any{
			"id": "response-prism-route", "model": "gpt-6.1-sol", "status": "completed", "usage": nil,
			"output": []any{map[string]any{
				"id": "message-prism-route", "type": "message", "role": "assistant", "status": "completed",
				"content": []any{map[string]any{"type": "output_text", "text": "route reached adapter"}},
			}},
		}
		if err := json.NewEncoder(w).Encode(response); err != nil {
			t.Error(err)
		}
	}))
	defer server.Close()
	endpoint, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("PRISM_ADAPTER_PORT", endpoint.Port())

	manager := coreauth.NewManager(nil, nil, nil)
	auth := &coreauth.Auth{
		ID: "prism-catalog-hot-reload", Provider: "codex", Status: coreauth.StatusActive,
		Attributes: map[string]string{"plan_type": "free"},
		Metadata: map[string]any{
			"auth_kind": coreauth.AuthKindOAuth, "access_token": "synthetic-oauth", "account_id": "synthetic-account",
		},
	}
	coreauth.SetOAuthModelAliasesAttribute(auth, []config.OAuthModelAlias{{Name: "gpt-6.1-sol", Alias: "prism-sol", Fork: true}})
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	service := &Service{cfg: &config.Config{}, coreManager: manager}
	service.completeModelRegistrationForAuthWithCache(t.Context(), auth, nil)
	baseline := codexModelIDSet(reg.GetModelsForClient(auth.ID))

	newConfig := func(enabled bool) *config.Config {
		return &config.Config{Codex: config.CodexConfig{Prism: internalconfig.CodexPrismConfig{
			Enabled: enabled,
		}}}
	}
	service.applyConfigUpdate(newConfig(false))
	older := service.commitConfigUpdate(newConfig(true))
	newer := service.commitConfigUpdate(newConfig(true))
	if service.applyConfigRuntime(t.Context(), older, false) {
		t.Fatal("stale config was applied")
	}
	if !service.applyConfigRuntime(t.Context(), newer, false) {
		t.Fatal("latest config was not applied")
	}
	for _, model := range append(registry.PrismModelIDs(), "prism-sol") {
		if !reg.ClientSupportsModel(auth.ID, model) {
			t.Fatalf("Prism model %q is not routable", model)
		}
		if _, ok := openAIModelIDSet(reg.GetAvailableModels("openai"))[model]; !ok {
			t.Fatalf("Prism model %q is missing from /v1/models", model)
		}
	}
	if !slices.Contains(reg.GetModelProviders("gpt-6.1-sol"), "codex") {
		t.Fatal("HTTP provider resolution cannot reach Prism")
	}
	for _, model := range []string{"gpt-6.1-sol(high)", "prism-sol(high)"} {
		payload, _ := json.Marshal(map[string]any{"model": model, "input": "hello"})
		response, err := manager.Execute(t.Context(), []string{"codex"}, core.Request{Model: model, Payload: payload}, core.Options{SourceFormat: translator.FormatOpenAIResponse})
		if err != nil {
			t.Fatalf("execute %s through scheduler: %v", model, err)
		}
		if gjson.GetBytes(response.Payload, "output.0.content.0.text").String() != "route reached adapter" {
			t.Fatalf("wrong response: %s", response.Payload)
		}
	}
	for _, cfg := range []*config.Config{newConfig(false)} {
		service.applyConfigUpdate(cfg)
		got := codexModelIDSet(reg.GetModelsForClient(auth.ID))
		if len(got) != len(baseline) {
			t.Fatalf("native catalog was not restored: got %v, want %v", got, baseline)
		}
		for id := range baseline {
			if _, ok := got[id]; !ok {
				t.Errorf("native model %q was removed", id)
			}
		}
		service.applyConfigUpdate(newConfig(true))
	}
}

func TestPrismCatalogRespectsUserExclusions(t *testing.T) {
	auth := &coreauth.Auth{
		ID: t.Name(), Provider: "codex", Status: coreauth.StatusActive,
		Metadata:   map[string]any{"auth_kind": coreauth.AuthKindOAuth},
		Attributes: map[string]string{"excluded_models": "gpt-6.1-sol"},
	}
	service := &Service{cfg: &config.Config{Codex: config.CodexConfig{Prism: internalconfig.CodexPrismConfig{Enabled: true}}}}
	reg := registry.GetGlobalRegistry()
	t.Cleanup(func() { reg.UnregisterClient(auth.ID) })
	service.registerModelsForAuth(t.Context(), auth)
	if reg.ClientSupportsModel(auth.ID, "gpt-6.1-sol") {
		t.Fatal("global model exclusion was ignored")
	}
	for _, model := range registry.PrismModelIDs()[1:] {
		if !reg.ClientSupportsModel(auth.ID, model) {
			t.Errorf("missing Prism model %q", model)
		}
	}
}
