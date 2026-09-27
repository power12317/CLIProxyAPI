package executor

import (
	"context"
	"io"
	"net/http"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
)

func (e *BasispointsExecutor) openWebsocket(ctx context.Context, auth *coreauth.Auth, request *basispoints.WebsocketRequest, upstreamLog helps.UpstreamRequestLog, reporter *helps.UsageReporter, observe func([]byte)) (*http.Response, error) {
	dial := e.websocketDial
	if dial == nil {
		dialer := newProxyAwareWebsocketDialer(ctx, e.cfg, auth)
		dial = func(dialCtx context.Context, target string, headers http.Header, protocols []string) (*websocket.Conn, *http.Response, error) {
			connectionDialer := *dialer
			connectionDialer.Subprotocols = protocols
			return connectionDialer.DialContext(dialCtx, target, headers)
		}
	}
	upstreamLog.URL = request.URL
	upstreamLog.Method = http.MethodGet
	upstreamLog.Headers = request.Headers.Clone()
	upstreamLog.Headers.Set("Sec-WebSocket-Protocol", "responses, [REDACTED]")
	upstreamLog.Body = request.LogFrame()
	for attempt := 0; attempt < coreexecutor.CodexWebsocketMaxFailures; attempt++ {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		helps.RecordAPIRequest(ctx, e.cfg, upstreamLog)
		reporter.StartResponseTTFT()
		response, sent, err := request.Open(ctx, dial, observe)
		if sent {
			coreexecutor.ReportUpstreamWebsocket(ctx, true)
			if err != nil {
				return nil, &coreexecutor.CodexReplayUnsafeError{Cause: err}
			}
			return response, nil
		}
		if response != nil {
			// Authentication, policy, validation and rate-limit responses remain
			// authoritative. In particular, a 403 enters the shared cooldown.
			status := response.StatusCode
			if status >= 400 && status < 500 && status != 404 && status != 405 && status != 426 {
				return response, nil
			}
			helps.RecordAPIResponseMetadata(ctx, e.cfg, status, response.Header)
			if response.Body != nil {
				raw, errRead := io.ReadAll(io.LimitReader(response.Body, 8<<20))
				if errRead == nil {
					helps.AppendAPIResponseChunk(ctx, e.cfg, raw)
				}
				if errClose := response.Body.Close(); errClose != nil {
					log.WithError(errClose).Debug("basispoints: close websocket handshake response")
				}
			}
		}
		helps.RecordAPIResponseError(ctx, e.cfg, err)
	}
	log.WithField("attempts", coreexecutor.CodexWebsocketMaxFailures).Warn("Basispoints WebSocket handshake failed; this request falls back to SSE")
	return nil, coreexecutor.ErrCodexWebsocketFallback
}
