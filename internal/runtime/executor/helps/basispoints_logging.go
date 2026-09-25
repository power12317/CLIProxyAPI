package helps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// RecordBasispointsFailure promotes this failed request to full local error
// logging even with request-log disabled. Request bodies and the failing event
// are complete; authorization headers are not copied into these diagnostics.
func RecordBasispointsFailure(ctx context.Context, cfg *config.Config, original, wire, upstream []byte, phase string, err error) {
	if cfg == nil || cfg.CommercialMode || err == nil || errors.Is(err, context.Canceled) {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx == nil {
		return
	}
	ginCtx.Set(logging.ForceErrorLogContextKey, true)
	if len(original) > 0 {
		ginCtx.Set("REQUEST_BODY_OVERRIDE", bytes.Clone(original))
	}
	fullLogging := *cfg
	fullLogging.RequestLog = true
	if !cfg.RequestLog || phase == "prepare" || phase == "attachment_upload" {
		RecordAPIRequest(ctx, &fullLogging, UpstreamRequestLog{
			URL: basispoints.ResponsesURL, Method: http.MethodPost, Body: wire, Provider: "codex",
		})
	}
	RecordAPIResponseError(ctx, &fullLogging, fmt.Errorf("Basispoints phase=%s: %w", phase, err))
	if !cfg.RequestLog && len(upstream) > 0 {
		AppendAPIResponseChunk(ctx, &fullLogging, upstream)
	}
	// Print the decoded transport input as well as its JSON envelope, so raw
	// newlines, quotes and the exact failing code need no manual unescaping.
	output := gjson.GetBytes(upstream, "response.output")
	if !output.IsArray() {
		output = gjson.GetBytes(upstream, "output")
	}
	var tools strings.Builder
	for _, item := range output.Array() {
		kind := item.Get("type").String()
		if kind != "function_call" && kind != "custom_tool_call" {
			continue
		}
		fmt.Fprintf(&tools, "\n=== BASISPOINTS TOOL INPUT ===\nName: %s\nCall ID: %s\n", item.Get("name").String(), item.Get("call_id").String())
		if kind == "custom_tool_call" {
			fmt.Fprintf(&tools, "Input:\n%s\n", item.Get("input").String())
			continue
		}
		arguments := item.Get("arguments")
		if arguments.Type == gjson.String {
			arguments = gjson.Parse(arguments.String())
		}
		fmt.Fprintf(&tools, "Arguments:\n%s\n", item.Get("arguments").String())
		if code := arguments.Get("code"); code.Type == gjson.String {
			fmt.Fprintf(&tools, "Transport code:\n%s\n", code.String())
		}
	}
	if tools.Len() > 0 {
		AppendAPIResponseChunk(ctx, &fullLogging, []byte(tools.String()))
	}
}

// NewBasispointsLogState uses the existing Codex access-log representation.
// The identity body is for logging only, never sent to Basispoints.
func NewBasispointsLogState(ctx context.Context, auth *coreauth.Auth, target string, original, wire []byte, clientHeaders http.Header, model string) *CodexTurnState {
	body := original
	nested := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")
	if nested.Type == gjson.String {
		nested = gjson.Parse(nested.String())
	}
	if gjson.GetBytes(body, "client_metadata.session_id").String() == "" && nested.Get("session_id").String() == "" {
		session := firstString(codexTurnHeaderValue(clientHeaders, "Session-Id"), codexTurnHeaderValue(clientHeaders, "Session_id"), gjson.GetBytes(body, "prompt_cache_key").String(), gjson.GetBytes(wire, "metadata.task_id").String())
		body, _ = sjson.SetBytes(body, "client_metadata.session_id", session)
	}
	if gjson.GetBytes(body, "client_metadata.turn_id").String() == "" && nested.Get("turn_id").String() == "" {
		body, _ = sjson.SetBytes(body, "client_metadata.turn_id", gjson.GetBytes(wire, "metadata.turn_id").String())
	}
	return NewCodexTurnState(ctx, auth, target, body, clientHeaders, model, clientHeaders)
}

// LogBasispointsRejection adds protocol diagnostics without logging request text,
// tool arguments, bearer tokens or encrypted reasoning contents.
func LogBasispointsRejection(ctx context.Context, status int, wire []byte, headers http.Header) {
	log.WithFields(log.Fields{
		"request_id": logging.GetRequestID(ctx), "status": status,
		"model":               gjson.GetBytes(wire, "model").String(),
		"reasoning_effort":    gjson.GetBytes(wire, "reasoning_effort").String(),
		"service_tier":        gjson.GetBytes(wire, "service_tier").String(),
		"input_items":         gjson.GetBytes(wire, "input.#").Int(),
		"upstream_request_id": headers.Get("X-Request-Id"),
	}).Warnf("Basispoints rejected request: status=%d effort=%q tier=%q input_items=%d upstream_request_id=%q", status, gjson.GetBytes(wire, "reasoning_effort").String(), gjson.GetBytes(wire, "service_tier").String(), gjson.GetBytes(wire, "input.#").Int(), headers.Get("X-Request-Id"))
}

func LogBasispointsNativeToolRoute(ctx context.Context, reason string) {
	log.WithFields(log.Fields{"request_id": logging.GetRequestID(ctx), "reason": reason}).Info("Basispoints hosted tool request uses native Codex")
}
