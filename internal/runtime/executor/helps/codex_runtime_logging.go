package helps

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"

	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

// CodexRuntimeLog routes the worker's actual native exchanges into CPA's existing logs.
type CodexRuntimeLog struct {
	ctx           context.Context
	cfg           *config.Config
	auth          *coreauth.Auth
	model         string
	turn          *CodexTurnState
	RawBody       bool
	pending       []byte
	eventData     [][]byte
	wait          func()
	client        *bridge.Client
	deferred      bool
	previous      func()
	previousOnce  sync.Once
	finishOnce    sync.Once
	executionDone chan struct{}
	executionOnce sync.Once
	errorMu       sync.Mutex
	errors        []error
}

func NewCodexRuntimeLog(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, model string) *CodexRuntimeLog {
	l := &CodexRuntimeLog{ctx: ctx, cfg: cfg, auth: auth, model: model, executionDone: make(chan struct{})}
	l.previous, l.deferred = logging.DeferDiagnostics(ctx, l.finish)
	return l
}

// Bind joins diagnostics after HTTP business delivery and before Gin reuses its
// context. Direct executor consumers join at Flush instead.
func (l *CodexRuntimeLog) Bind(client *bridge.Client) {
	l.client = client
	l.wait = client.WaitDiagnostics
}

func (l *CodexRuntimeLog) Record(entry bridge.UpstreamLog) {
	l.joinPrevious()
	switch entry.Kind {
	case "request":
		l.flush()
		if reporter, _ := l.ctx.Value(codexOaiLBReporterKey{}).(*UsageReporter); reporter != nil && entry.AccessTokenSHA256 != "" {
			reporter.authMu.Lock()
			reporter.accessTokenHash = entry.AccessTokenSHA256
			reporter.authMu.Unlock()
		}
		info := UpstreamRequestLog{URL: entry.URL, Method: entry.Method, Headers: entry.Headers, Body: codexRuntimeLogRequestBody(entry), Provider: "codex", OaiLBNode: entry.OaiLBNode}
		if l.auth != nil {
			info.AuthID, info.AuthLabel = l.auth.ID, l.auth.Label
			info.AuthType, info.AuthValue = l.auth.AccountInfo()
		}
		RecordAPIRequest(l.ctx, l.cfg, info)
		coreexecutor.MarkUpstreamAttempt(l.ctx)
		l.turn = codexTurnStateFromRequest(l.auth, entry.URL, info.Body, entry.Headers, entry.Transport == "websocket")
		l.turn.ctx, l.turn.requestedModel = l.ctx, l.model
		l.turn.updateLogFields(true)
		var websocketBody []byte
		if entry.Transport == "websocket" {
			websocketBody = info.Body
		}
		l.turn.ObserveRequest(entry.Headers, websocketBody)
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
		l.flush()
		RecordAPIResponseError(l.ctx, l.cfg, errors.New(entry.Message))
	case "body":
		if entry.Transport == "websocket" {
			l.flush()
			AppendAPIResponseChunk(l.ctx, l.cfg, entry.BodyBytes)
			if l.turn != nil {
				l.turn.ObserveEvent(entry.BodyBytes)
			}
			return
		}
		l.pending = append(l.pending, entry.BodyBytes...)
		for {
			index := bytes.IndexByte(l.pending, '\n')
			if index < 0 {
				break
			}
			AppendAPIResponseChunk(l.ctx, l.cfg, l.pending[:index])
			l.observeLine(l.pending[:index])
			l.pending = l.pending[index+1:]
		}
	}
}

// Decode a copy for the existing readable request log; native transport bytes
// and compression settings are never changed by logging.
func codexRuntimeLogRequestBody(entry bridge.UpstreamLog) []byte {
	if len(entry.BodyBytes) == 0 {
		return []byte(entry.Body)
	}
	if strings.EqualFold(entry.Headers.Get("Content-Encoding"), "zstd") {
		decoder, err := zstd.NewReader(nil)
		if err == nil {
			decoded, errDecode := decoder.DecodeAll(entry.BodyBytes, nil)
			decoder.Close()
			if errDecode == nil {
				return decoded
			}
		}
	}
	return entry.BodyBytes
}

func (l *CodexRuntimeLog) Event(event []byte) {
	if !l.RawBody {
		AppendAPIResponseChunk(l.ctx, l.cfg, append([]byte("data: "), event...))
		if l.turn != nil {
			l.turn.ObserveEvent(event)
		}
	}
}

func (l *CodexRuntimeLog) observeLine(line []byte) {
	line = bytes.TrimSuffix(line, []byte("\r"))
	if bytes.HasPrefix(line, []byte("data:")) {
		l.eventData = append(l.eventData, bytes.TrimPrefix(line[5:], []byte(" ")))
	} else if len(line) == 0 {
		l.observeEventData()
	}
}

func (l *CodexRuntimeLog) observeEventData() {
	if l.turn != nil && len(l.eventData) > 0 {
		l.turn.ObserveEvent(bytes.Join(l.eventData, []byte("\n")))
	}
	l.eventData = nil
}

// Flush preserves a final partial wire line on EOF, errors and cancellation.
func (l *CodexRuntimeLog) Flush() {
	l.executionOnce.Do(func() { close(l.executionDone) })
	if !l.deferred {
		l.finish()
	}
}

func (l *CodexRuntimeLog) finish() {
	l.finishOnce.Do(func() {
		<-l.executionDone
		l.joinPrevious()
		if l.wait != nil {
			l.wait()
		}
		l.flush()
		if reporter, _ := l.ctx.Value(codexOaiLBReporterKey{}).(*UsageReporter); reporter != nil {
			logging.UpdateCodexTurnStateLogModels(ginContextFrom(l.ctx), l.model, reporter.ResponseModel())
		}
		l.errorMu.Lock()
		defer l.errorMu.Unlock()
		for _, err := range l.errors {
			RecordAPIResponseError(l.ctx, l.cfg, err)
		}
	})
}

func (l *CodexRuntimeLog) joinPrevious() {
	l.previousOnce.Do(func() {
		if l.previous != nil {
			l.previous()
			l.previous = nil
		}
	})
}

func (l *CodexRuntimeLog) flush() {
	if len(l.pending) > 0 {
		AppendAPIResponseChunk(l.ctx, l.cfg, l.pending)
		l.observeLine(l.pending)
		l.pending = nil
	}
	l.observeEventData()
}

func (l *CodexRuntimeLog) Error(err error) {
	if l.client != nil && l.client.NativeCompleted() && (errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)) {
		err = fmt.Errorf("downstream delivery interrupted after native model completion: %w", err)
	}
	if l.wait == nil && !l.deferred {
		l.flush()
		RecordAPIResponseError(l.ctx, l.cfg, err)
		return
	}
	l.errorMu.Lock()
	l.errors = append(l.errors, err)
	l.errorMu.Unlock()
}
