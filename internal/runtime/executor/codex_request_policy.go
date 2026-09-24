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

func codexResponsesLiteBodyMode(body []byte, headers http.Header) bool {
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

// ensureCodexResponsesLiteHeader restores explicit body intent, including false.
func ensureCodexResponsesLiteHeader(headers http.Header, body []byte) {
	if headers == nil || headers.Get(codexResponsesLiteHeader) != "" {
		return
	}
	if value := util.CodexResponsesLiteHeaderValue(body, nil); value != "" {
		headers.Set(codexResponsesLiteHeader, value)
	}
}

// ensureCodexResponsesLiteMirror keeps every WS frame aligned with the resolved
// header. A reused connection cannot update its handshake on subsequent turns.
func ensureCodexResponsesLiteMirror(body []byte, headers http.Header) []byte {
	value := util.CodexResponsesLiteHeaderValue(body, headers)
	if !strings.EqualFold(value, "true") && !strings.EqualFold(value, "false") {
		return body
	}
	if current := gjson.GetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite"); current.Exists() && current.String() == strings.ToLower(value) {
		return body
	}
	updated, errSet := sjson.SetBytes(body, "client_metadata.ws_request_header_x_openai_internal_codex_responses_lite", strings.ToLower(value))
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

func applyCodexImageGenerationPolicy(body []byte, baseModel string, auth *cliproxyauth.Auth, cfg *config.Config, headers http.Header, requestPath string, official bool, originalRequests ...[]byte) ([]byte, error) {
	return applyCodexImageGenerationPolicyWithInjection(body, baseModel, auth, cfg, headers, requestPath, official, true, originalRequests...)
}

func applyCodexImageGenerationPolicyWithoutInjection(body []byte, cfg *config.Config, requestPath string) []byte {
	if codexShouldStripImageGeneration(cfg, requestPath) {
		return stripCodexImageGenerationTools(body)
	}
	return body
}

func applyCodexImageGenerationPolicyWithInjection(body []byte, baseModel string, auth *cliproxyauth.Auth, cfg *config.Config, headers http.Header, requestPath string, official, allowInjection bool, originalRequests ...[]byte) ([]byte, error) {
	if cfg != nil && cfg.DisableImageGeneration == config.DisableImageGenerationPassthrough {
		return body, nil
	}
	stripImages := codexShouldStripImageGeneration(cfg, requestPath)
	if stripImages {
		body = stripCodexImageGenerationTools(body)
	}
	if !allowInjection {
		return body, nil
	}
	injectImages := !stripImages && (cfg == nil || cfg.DisableImageGeneration == config.DisableImageGenerationOff) && !isCodexFreePlanAuth(auth) && !strings.HasSuffix(baseModel, "spark")
	if codexResponsesLiteBodyMode(body, headers) {
		return helps.NormalizeCodexLiteCompatibilityTools(body, injectImages, originalRequests...)
	}
	if !injectImages {
		return body, nil
	}
	return ensureImageGenerationTool(body, baseModel, auth), nil
}
