package executor

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestCodexUnifiedImagePolicy(t *testing.T) {
	for _, official := range []bool{false, true} {
		for _, lite := range []bool{false, true} {
			for _, mode := range []config.DisableImageGenerationMode{config.DisableImageGenerationOff, config.DisableImageGenerationAll, config.DisableImageGenerationChat, config.DisableImageGenerationPassthrough} {
				for _, existing := range []bool{false, true} {
					t.Run(fmt.Sprintf("official=%t/lite=%t/mode=%s/existing=%t", official, lite, mode, existing), func(t *testing.T) {
						body := []byte(`{"model":"gpt-5.6-sol","input":[],"reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`)
						headers := make(http.Header)
						headers.Set(codexResponsesLiteHeader, fmt.Sprint(lite))
						if existing {
							body, _ = sjson.SetRawBytes(body, "tools", []byte(`[{"type":"image_generation"},{"type":"function","name":"exec","parameters":{}}]`))
							body, _ = sjson.SetRawBytes(body, "tool_choice", []byte(`{"type":"image_generation"}`))
						}
						cfg := &config.Config{SDKConfig: config.SDKConfig{DisableImageGeneration: mode}}
						got := applyCodexImageGenerationPolicy(body, "gpt-5.6-sol", nil, cfg, headers, "/v1/responses", official)
						if mode == config.DisableImageGenerationPassthrough {
							if string(got) != string(body) {
								t.Fatalf("passthrough changed tools: %s", got)
							}
							return
						}
						if helps.HasCodexImageTool(got) != (mode == config.DisableImageGenerationOff) {
							t.Fatalf("incorrect image policy: %s", got)
						}
						if lite && (gjson.GetBytes(got, "tools").Exists() || util.ClassifyCodexResponsesLiteTools(got) == util.CodexResponsesLiteToolsIncompatible) {
							t.Fatalf("invalid Lite declarations: %s", got)
						}
						tools := util.CollectResponsesToolWinners(gjson.ParseBytes(got))
						if _, exists := tools["web__run"]; exists {
							t.Fatalf("image setting added unsolicited search: %s", got)
						}
						if existing && tools["exec"].ToolType != "function" {
							t.Fatalf("unrelated tool lost: %s", got)
						}
						if mode != config.DisableImageGenerationOff && gjson.GetBytes(got, "tool_choice").Exists() {
							t.Fatalf("disabled image choice survived: %s", got)
						}
						if !existing && gjson.GetBytes(got, "tool_choice").Exists() {
							t.Fatalf("injection forced tool execution: %s", got)
						}
					})
				}
			}
		}
	}
}

func TestCodexUnifiedImagePolicyRetainsExistingDeclarations(t *testing.T) {
	for _, declaration := range []string{
		`{"type":"image_generation","quality":"high"}`,
		`{"type":"function","name":"image_gen.imagegen","description":"client schema"}`,
		`{"type":"function","name":"image_gen__imagegen","description":"client schema"}`,
		`{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen","description":"client schema"}]}`,
	} {
		body := []byte(`{"model":"gpt-5.5","input":[{"id":"at-client","type":"additional_tools","tools":[]}],"tool_choice":"auto"}`)
		body, _ = sjson.SetRawBytes(body, "input.0.tools.-1", []byte(declaration))
		got := applyCodexImageGenerationPolicy(body, "gpt-5.5", nil, &config.Config{}, nil, "/v1/responses", true)
		if string(body) != string(got) {
			t.Fatalf("existing additional_tools declaration changed: %s", got)
		}
	}
}

func TestCodexUnifiedImagePolicyRetainsFreeSparkAndCompactExclusions(t *testing.T) {
	headers := http.Header{http.CanonicalHeaderKey(codexResponsesLiteHeader): {"true"}}
	for _, lite := range []bool{false, true} {
		var source http.Header
		if lite {
			source = headers
		}
		for _, test := range []struct {
			model string
			auth  *cliproxyauth.Auth
		}{
			{"gpt-5.6-sol", &cliproxyauth.Auth{Provider: "codex", Attributes: map[string]string{"plan_type": "free"}}},
			{"gpt-5.3-codex-spark", nil},
		} {
			got := applyCodexImageGenerationPolicy([]byte(`{"input":[]}`), test.model, test.auth, &config.Config{}, source, "/v1/responses", true)
			if helps.HasCodexImageTool(got) {
				t.Fatalf("excluded request gained image tool: %s", got)
			}
		}
	}
	body := []byte(`{"input":[],"instructions":"compact"}`)
	if got := applyCodexImageGenerationPolicyWithoutInjection(body, &config.Config{}, "/v1/responses/compact"); string(got) != string(body) {
		t.Fatalf("compact gained declarations: %s", got)
	}
}

func TestCodexToolPolicyUsesConfiguredLiteOverride(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-sol","input":[],"reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`)
	for _, enabled := range []bool{false, true} {
		source := http.Header{http.CanonicalHeaderKey(codexResponsesLiteHeader): {fmt.Sprint(!enabled)}}
		auth := &cliproxyauth.Auth{Attributes: map[string]string{"header:" + codexResponsesLiteHeader: fmt.Sprint(enabled)}}
		headers := codexToolPolicyHeaders(auth, source, "gpt-5.6-sol")
		got := applyCodexImageGenerationPolicy(body, "gpt-5.6-sol", auth, &config.Config{}, headers, "/v1/responses", true)
		if headers.Get(codexResponsesLiteHeader) != fmt.Sprint(enabled) || source.Get(codexResponsesLiteHeader) != fmt.Sprint(!enabled) {
			t.Fatalf("incorrect override/source mutation: %v %v", headers, source)
		}
		if hosted := gjson.GetBytes(got, "tools.0.type").String() == "image_generation"; hosted == enabled || !helps.HasCodexImageTool(got) {
			t.Fatalf("tool shape conflicts with configured header: %s", got)
		}
	}
}
