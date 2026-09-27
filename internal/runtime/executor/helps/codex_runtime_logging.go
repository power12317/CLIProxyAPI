package helps

import (
	"context"
	"errors"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
)

// CodexRuntimeLog routes the worker's actual HTTP exchanges into CPA's existing logs.
type CodexRuntimeLog struct {
	ctx   context.Context
	cfg   *config.Config
	auth  *coreauth.Auth
	model string
	turn  *CodexTurnState
}

func NewCodexRuntimeLog(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, model string) *CodexRuntimeLog {
	return &CodexRuntimeLog{ctx: ctx, cfg: cfg, auth: auth, model: model}
}

func (l *CodexRuntimeLog) Record(entry bridge.UpstreamLog) {
	switch entry.Kind {
	case "request":
		info := UpstreamRequestLog{URL: entry.URL, Method: entry.Method, Headers: entry.Headers, Body: []byte(entry.Body), Provider: "codex"}
		if l.auth != nil {
			info.AuthID, info.AuthLabel = l.auth.ID, l.auth.Label
			info.AuthType, info.AuthValue = l.auth.AccountInfo()
		}
		RecordAPIRequest(l.ctx, l.cfg, info)
		coreexecutor.MarkUpstreamAttempt(l.ctx)
		l.turn = codexTurnStateFromRequest(l.auth, entry.URL, info.Body, entry.Headers, false)
		l.turn.ctx, l.turn.requestedModel = l.ctx, l.model
		l.turn.updateLogFields(true)
		l.turn.ObserveRequest(entry.Headers, nil)
		RecordCodexOaiLBNode(l.ctx, entry.OaiLBNode)
	case "response":
		RecordAPIResponseMetadata(l.ctx, l.cfg, entry.StatusCode, entry.Headers)
		RecordCodexOaiLBNode(l.ctx, entry.OaiLBNode)
		if l.turn != nil {
			l.turn.ObserveResponse(&http.Response{Header: entry.Headers})
			l.turn.LogResponse(l.ctx, l.cfg, false)
		}
		if entry.Body != "" {
			AppendAPIResponseChunk(l.ctx, l.cfg, []byte(entry.Body))
		}
	case "error":
		RecordAPIResponseError(l.ctx, l.cfg, errors.New(entry.Message))
	}
}

func (l *CodexRuntimeLog) Event(event []byte) {
	AppendAPIResponseChunk(l.ctx, l.cfg, append([]byte("data: "), event...))
	if l.turn != nil {
		l.turn.ObserveEvent(event)
	}
}
