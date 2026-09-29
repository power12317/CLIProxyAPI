package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

// readWebsocket retries authoritative upstream failures on the same socket.
// The outer replay guard still prevents credential retries from multiplying this budget.
func (e *BasispointsExecutor) readWebsocket(ctx context.Context, auth *coreauth.Auth, response *http.Response, bridge *basispoints.Bridge, stream, bufferOutput bool, emit func([]byte) error) error {
	limit, maxWait := 0, time.Duration(0)
	if e.cfg != nil {
		limit = max(e.cfg.RequestRetry, 0)
		maxWait = time.Duration(e.cfg.MaxRetryInterval) * time.Second
	}
	if override, ok := auth.RequestRetryOverride(); ok {
		limit = override
	}
	retry, ok := response.Body.(interface{ Retry() error })
	if !ok || limit == 0 {
		return bridge.Stream(response.Body, emit)
	}
	outputStarted := false
	for attempt := 0; ; attempt++ {
		var buffered [][]byte
		bufferedBytes := 0
		err := bridge.Stream(response.Body, func(event []byte) error {
			if !stream {
				return emit(event)
			}
			// The existing optional-tool path publishes only a successful candidate.
			// Retain that boundary per attempt so rejected text cannot leak on retry.
			if bufferOutput {
				buffered = append(buffered, bytes.Clone(event))
				return nil
			}
			var payload []byte
			if index := bytes.Index(event, []byte("data:")); index >= 0 {
				payload = bytes.TrimSpace(event[index+len("data:"):])
			}
			kind := gjson.GetBytes(payload, "type").String()
			if !outputStarted && isCodexBootstrapBufferableEvent(kind, payload) && len(buffered) < codexBootstrapMaxBufferedFrames && bufferedBytes+len(event) <= codexBootstrapMaxBufferedBytes {
				buffered = append(buffered, bytes.Clone(event))
				bufferedBytes += len(event)
				return nil
			}
			outputStarted = true
			for _, preamble := range buffered {
				if errEmit := emit(preamble); errEmit != nil {
					return errEmit
				}
			}
			buffered = nil
			return emit(event)
		})
		if err == nil {
			if stream && bufferOutput {
				for _, event := range buffered {
					if errEmit := emit(event); errEmit != nil {
						return errEmit
					}
				}
			}
			return nil
		}
		if outputStarted || attempt >= limit {
			return err
		}
		var upstream *basispoints.Error
		if !errors.As(err, &upstream) || upstream.Body != string(bridge.LastEvent) {
			return err
		}
		if !coreauth.IsRequestRetryable(auth, err, e.cfg) {
			return err
		}
		if delay := upstream.RetryAfter(); delay != nil && *delay > 0 {
			if maxWait <= 0 || *delay > maxWait {
				return err
			}
			timer := time.NewTimer(*delay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
		if errRetry := retry.Retry(); errRetry != nil {
			return errRetry
		}
	}
}

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
	request.ObserveRequest = func(frame []byte) {
		upstreamLog.Body = frame
		helps.RecordAPIRequest(ctx, e.cfg, upstreamLog)
	}
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
