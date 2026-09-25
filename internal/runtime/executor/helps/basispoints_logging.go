package helps

import (
	"context"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

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
