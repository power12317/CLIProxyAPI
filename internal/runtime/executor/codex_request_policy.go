package executor

import (
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func codexOfficialRequest(originalPayload, payload []byte) bool {
	return helps.IsOfficialCodexRequest(originalPayload) || helps.IsOfficialCodexRequest(payload)
}

func codexResponsesLiteBodyMode(body []byte, official bool, headers http.Header) bool {
	if explicit := strings.TrimSpace(headers.Get(codexResponsesLiteHeader)); explicit != "" {
		return strings.EqualFold(explicit, "true")
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if official && codexResponsesLiteModelEnabled(model) && codexResponsesLiteBodyFieldsSatisfied(body) {
		return true
	}
	return util.IsCodexResponsesLiteRequest(body, headers)
}

// codexToolPolicyHeaders uses the same explicit Lite overrides as the final
// request, so tool normalization cannot choose a conflicting wire format.
func codexToolPolicyHeaders(auth *cliproxyauth.Auth, source http.Header, model string) http.Header {
	headers := source.Clone()
	if headers == nil {
		headers = make(http.Header)
	}
	applyCodexConfiguredHeaderOverrides(&http.Request{Header: headers}, auth, source)
	for key, value := range registry.ModelOverrideHeaders(model) {
		if strings.EqualFold(key, codexResponsesLiteHeader) {
			headers.Set(key, value)
		}
	}
	return headers
}

func codexResponsesLiteModelEnabled(model string) bool {
	return registry.CodexModelUsesResponsesLite(model)
}

func codexResponsesLiteBodyFieldsSatisfied(body []byte) bool {
	contextValue := gjson.GetBytes(body, "reasoning.context")
	if contextValue.Type != gjson.String || contextValue.String() != "all_turns" {
		return false
	}
	parallelValue := gjson.GetBytes(body, "parallel_tool_calls")
	return parallelValue.Type == gjson.False
}

func codexResponsesLiteAutoEnabled(body []byte, model string, official bool) bool {
	return official && codexResponsesLiteModelEnabled(model) && codexResponsesLiteBodyFieldsSatisfied(body) && util.ClassifyCodexResponsesLiteTools(body) != util.CodexResponsesLiteToolsIncompatible
}

// ensureCodexResponsesLiteHeader only reconstructs a missing header for an
// official Codex body. Existing client or configured headers are preserved.
func ensureCodexResponsesLiteHeader(headers http.Header, body []byte, model string, official bool) {
	if headers == nil || headers.Get(codexResponsesLiteHeader) != "" {
		return
	}
	if codexResponsesLiteAutoEnabled(body, model, official) || util.IsCodexResponsesLiteRequest(body, nil) {
		headers.Set(codexResponsesLiteHeader, "true")
	}
}

func ensureCodexResponsesLiteMirror(body []byte, model string, official bool, headerSets ...http.Header) []byte {
	var headers http.Header
	if len(headerSets) > 0 {
		headers = headerSets[0]
	}
	if explicit := strings.TrimSpace(headers.Get(codexResponsesLiteHeader)); explicit != "" && !strings.EqualFold(explicit, "true") {
		return body
	}
	if !codexResponsesLiteAutoEnabled(body, model, official) && !util.IsCodexResponsesLiteRequest(body, headers) {
		return body
	}
	if gjson.GetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite").Exists() {
		return body
	}
	updated, errSet := sjson.SetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite", "true")
	if errSet != nil {
		return body
	}
	return updated
}

func codexShouldStripImageGeneration(cfg *config.Config, requestPath string) bool {
	mode := config.DisableImageGenerationOff
	if cfg != nil {
		mode = cfg.DisableImageGeneration
	}
	switch mode {
	case config.DisableImageGenerationAll:
		return true
	case config.DisableImageGenerationChat:
		return !codexImagesEndpointPath(requestPath)
	default:
		return false
	}
}

func codexImagesEndpointPath(requestPath string) bool {
	path := strings.TrimSpace(requestPath)
	return path == "/v1/images/generations" || path == "/v1/images/edits" ||
		strings.HasSuffix(path, "/v1/images/generations") || strings.HasSuffix(path, "/v1/images/edits") ||
		strings.HasSuffix(path, "/images/generations") || strings.HasSuffix(path, "/images/edits")
}

// stripCodexImageGenerationTools removes supported image-generation tool forms
// from top-level and additional_tools arrays while preserving other namespace tools.
func stripCodexImageGenerationTools(body []byte) []byte { return helps.StripCodexImageTools(body) }

func applyCodexImageGenerationPolicy(body []byte, baseModel string, auth *cliproxyauth.Auth, cfg *config.Config, headers http.Header, requestPath string, official bool) []byte {
	return applyCodexImageGenerationPolicyWithInjection(body, baseModel, auth, cfg, headers, requestPath, official, true)
}

func applyCodexImageGenerationPolicyWithoutInjection(body []byte, cfg *config.Config, requestPath string) []byte {
	return applyCodexImageGenerationPolicyWithInjection(body, "", nil, cfg, nil, requestPath, false, false)
}

func applyCodexImageGenerationPolicyWithInjection(body []byte, baseModel string, auth *cliproxyauth.Auth, cfg *config.Config, headers http.Header, requestPath string, official, allowInjection bool) []byte {
	if cfg != nil && cfg.DisableImageGeneration == config.DisableImageGenerationPassthrough {
		return body
	}
	stripImages := codexShouldStripImageGeneration(cfg, requestPath)
	if stripImages {
		body = stripCodexImageGenerationTools(body)
	}
	if !allowInjection {
		return body
	}
	injectImages := !stripImages && (cfg == nil || cfg.DisableImageGeneration == config.DisableImageGenerationOff) && !isCodexFreePlanAuth(auth) && !strings.HasSuffix(baseModel, "spark")
	if codexResponsesLiteBodyMode(body, official, headers) {
		return helps.NormalizeCodexLiteCompatibilityTools(body, injectImages)
	}
	if !injectImages {
		return body
	}
	return ensureImageGenerationTool(body, baseModel, auth)
}
