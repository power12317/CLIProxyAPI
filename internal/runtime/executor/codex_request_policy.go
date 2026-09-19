package executor

import (
	"encoding/json"
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
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if official && registry.CodexModelUsesResponsesLite(model) {
		return true
	}
	return util.IsCodexResponsesLiteRequest(body, headers)
}

func codexResponsesLiteModelEnabled(model string) bool {
	return registry.CodexModelUsesResponsesLite(model)
}

// ensureCodexResponsesLiteHeader only reconstructs a missing header for an
// official Codex body. Existing client or configured headers are preserved.
func ensureCodexResponsesLiteHeader(headers http.Header, body []byte, model string, official bool) {
	if headers == nil || headers.Get(codexResponsesLiteHeader) != "" {
		return
	}
	if official && registry.CodexModelUsesResponsesLite(model) {
		headers.Set(codexResponsesLiteHeader, "true")
	}
}

func ensureCodexResponsesLiteMirror(body []byte, model string, official bool) []byte {
	if !official || !registry.CodexModelUsesResponsesLite(model) {
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
func stripCodexImageGenerationTools(body []byte) []byte {
	var document any
	if errUnmarshal := json.Unmarshal(body, &document); errUnmarshal != nil {
		return body
	}
	changed := false
	var walk func(any) any
	var cleanTool func(any) (any, bool)

	cleanTool = func(value any) (any, bool) {
		tool, ok := value.(map[string]any)
		if !ok {
			return walk(value), false
		}
		typ, _ := tool["type"].(string)
		name, _ := tool["name"].(string)
		if typ == "image_generation" || typ == "function" && name == "image_gen.imagegen" {
			return nil, true
		}
		if typ == "namespace" {
			if nested, okNested := tool["tools"].([]any); okNested {
				filtered := make([]any, 0, len(nested))
				for _, child := range nested {
					cleaned, remove := cleanTool(child)
					if remove {
						changed = true
						continue
					}
					filtered = append(filtered, cleaned)
				}
				if len(filtered) == 0 && name == "image_gen" {
					return nil, true
				}
				tool["tools"] = filtered
			}
		}
		return walk(tool), false
	}

	walk = func(value any) any {
		switch typed := value.(type) {
		case []any:
			for index := range typed {
				typed[index] = walk(typed[index])
			}
			return typed
		case map[string]any:
			for key, child := range typed {
				if key == "tools" {
					if tools, ok := child.([]any); ok {
						filtered := make([]any, 0, len(tools))
						for _, tool := range tools {
							cleaned, remove := cleanTool(tool)
							if remove {
								changed = true
								continue
							}
							filtered = append(filtered, cleaned)
						}
						typed[key] = filtered
						continue
					}
				}
				if key == "tool_choice" && isCodexImageGenerationToolChoice(child) {
					delete(typed, key)
					changed = true
					continue
				}
				typed[key] = walk(child)
			}
			return typed
		default:
			return value
		}
	}

	document = walk(document)
	if !changed {
		return body
	}
	encoded, errMarshal := json.Marshal(document)
	if errMarshal != nil {
		return body
	}
	return encoded
}

func isCodexImageGenerationToolChoice(value any) bool {
	switch typed := value.(type) {
	case string:
		return typed == "image_generation" || typed == "image_gen.imagegen"
	case map[string]any:
		typ, _ := typed["type"].(string)
		name, _ := typed["name"].(string)
		return typ == "image_generation" || name == "image_generation" || name == "image_gen.imagegen"
	default:
		return false
	}
}

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
	if codexShouldStripImageGeneration(cfg, requestPath) {
		return stripCodexImageGenerationTools(body)
	}
	if official || !allowInjection {
		return body
	}
	if cfg == nil || cfg.DisableImageGeneration == config.DisableImageGenerationOff {
		return ensureImageGenerationTool(body, baseModel, auth, headers)
	}
	return body
}
