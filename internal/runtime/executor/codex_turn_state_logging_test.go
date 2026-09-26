package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	coreusage "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/usage"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestCodexFinalTurnStateLengthsInLogsAndUsage(t *testing.T) {
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() { logger.ReplaceHooks(previousHooks); logger.SetLevel(previousLevel) })

	for _, transport := range []string{"http", "sse", "compact", "websocket", "websocket-stream"} {
		for _, tc := range []struct {
			name          string
			plan          string
			model         string
			responseModel string
			ticketLen     int
			passiveLen    int
			responseLen   int
			missingTurn   bool
		}{
			{name: "personal-ticket", model: "gpt-6-astra", ticketLen: 780},
			{name: "substituted-model", model: "gpt-6-sol", responseModel: "gpt-5.6-luna", responseLen: 780},
			{name: "retained-ticket-replaced", model: "gpt-6-astra", ticketLen: 780, responseLen: 780},
			{name: "312-preserves-ticket", model: "gpt-6-astra", ticketLen: 780, responseLen: 312},
			{name: "replace-passive-312", model: "gpt-6-astra", ticketLen: 780, passiveLen: 312},
			{name: "team-ticket", plan: "team", model: "gpt-5.6-sol", ticketLen: 780},
			{name: "unconfigured-model", model: "gpt-5.6-luna", responseLen: 312},
			{name: "luna-normal-response-ticket", model: "gpt-5.6-luna", responseLen: 780},
			{name: "terra-normal-response-ticket", model: "gpt-5.6-terra", responseLen: 780},
			{name: "team-normal-response-ticket", plan: "team", model: "gpt-5.6-luna", responseLen: 780},
			{name: "passive-without-ticket", model: "gpt-6-astra", passiveLen: 312},
			{name: "ticket-without-turn", model: "gpt-6-astra", ticketLen: 780, missingTurn: true},
		} {
			t.Run(transport+"/"+tc.name, func(t *testing.T) {
				isWebsocket := strings.HasPrefix(transport, "websocket")
				captured := make(chan string, 2)
				var handshakes atomic.Int32
				servedModel := tc.responseModel
				if servedModel == "" {
					servedModel = tc.model
				}
				response := `{"id":"resp_1","model":"` + servedModel + `","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
				if transport == "compact" {
					// Compaction responses do not report a served model.
					servedModel = ""
					response = `{"id":"resp_1","object":"response.compaction","output":[],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`
				}
				completed := `{"type":"response.completed","response":` + response + `}`
				responseState := strings.Repeat("r", tc.responseLen)
				server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					if isWebsocket {
						handshakes.Add(1)
						upgrader := websocket.Upgrader{}
						conn, errUpgrade := upgrader.Upgrade(w, r, nil)
						if errUpgrade != nil {
							t.Error(errUpgrade)
							return
						}
						defer func() { _ = conn.Close() }()
						for {
							_, body, errRead := conn.ReadMessage()
							if errRead != nil {
								return
							}
							captured <- gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String()
							if tc.responseLen > 0 {
								metadata, _ := json.Marshal(map[string]any{"type": "response.metadata", "headers": map[string]string{"x-codex-turn-state": responseState}})
								if errWrite := conn.WriteMessage(websocket.TextMessage, metadata); errWrite != nil {
									t.Error(errWrite)
									return
								}
							}
							if errWrite := conn.WriteMessage(websocket.TextMessage, []byte(completed)); errWrite != nil {
								t.Error(errWrite)
								return
							}
						}
					}
					_, _ = io.Copy(io.Discard, r.Body)
					captured <- r.Header.Get(helps.CodexTurnStateTicketHeader)
					if tc.responseLen > 0 {
						w.Header().Set(helps.CodexTurnStateTicketHeader, responseState)
					}
					if transport == "compact" {
						w.Header().Set("Content-Type", "application/json")
						_, _ = io.WriteString(w, response)
					} else {
						w.Header().Set("Content-Type", "text/event-stream")
						_, _ = fmt.Fprintf(w, "data: %s\n\n", completed)
					}
				}))
				defer server.Close()
				cfg := &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true}}}
				auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Attributes: map[string]string{
					"base_url": server.URL, "plan_type": tc.plan, cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth,
				}, Metadata: map[string]any{"access_token": "test-token"}}
				t.Cleanup(func() { helps.InvalidateCodexTurnStates(auth.ID); helps.InvalidateCodexCookieJar(auth.ID) })
				if tc.ticketLen > 0 {
					helps.StoreCodexTurnStateTicket(auth, helps.CodexTurnStateTicket{Model: tc.model, State: "gAAAAA" + strings.Repeat("t", tc.ticketLen-6), ExpiresAt: time.Now().Add(time.Hour)})
				}
				payload := []byte(`{"model":"` + tc.model + `","input":[],"client_metadata":{"turn_id":"turn-1"}}`)
				if tc.missingTurn {
					payload = []byte(`{"model":"` + tc.model + `","input":[]}`)
				}
				if tc.passiveLen > 0 {
					passive := helps.NewCodexTurnState(context.Background(), auth, server.URL+"/responses", payload, nil, tc.model, nil)
					passive.ObserveResponse(&http.Response{Header: http.Header{helps.CodexTurnStateTicketHeader: {strings.Repeat("p", tc.passiveLen)}}})
				}
				capture := &codexResponseModelUsageCapture{alias: t.Name(), records: make(chan coreusage.Record, 4)}
				coreusage.RegisterNamedPlugin(t.Name(), capture)
				t.Cleanup(func() { coreusage.RegisterNamedPlugin(t.Name(), codexResponseModelNoopUsagePlugin{}) })
				httpExec := NewCodexExecutor(cfg)
				wsExec := NewCodexWebsocketsExecutor(cfg)
				defer wsExec.CloseExecutionSession(t.Name())
				engine := gin.New()
				engine.Use(logging.GinLogrusLogger())
				engine.POST("/v1/responses", func(c *gin.Context) {
					ctx := coreusage.WithRequestedModelAlias(context.WithValue(c.Request.Context(), "gin", c), t.Name())
					req := cliproxyexecutor.Request{Model: tc.model, Payload: payload}
					opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Metadata: map[string]any{cliproxyexecutor.ExecutionSessionMetadataKey: t.Name()}}
					opts.Stream = transport == "sse" || transport == "websocket-stream"
					var errExecute error
					if transport == "sse" || transport == "websocket-stream" {
						executeStream := httpExec.ExecuteStream
						if isWebsocket {
							executeStream = wsExec.ExecuteStream
						}
						result, errStream := executeStream(ctx, auth, req, opts)
						errExecute = errStream
						if errStream == nil {
							for chunk := range result.Chunks {
								if chunk.Err != nil {
									errExecute = chunk.Err
								}
							}
						}
					} else {
						execute := httpExec.Execute
						if isWebsocket {
							execute = wsExec.Execute
						}
						if transport == "compact" {
							opts.Alt = "responses/compact"
						}
						_, errExecute = execute(ctx, auth, req, opts)
					}
					if errExecute != nil {
						t.Error(errExecute)
						c.Status(http.StatusBadGateway)
						return
					}
					c.Status(http.StatusOK)
				})
				attempts := 1
				if isWebsocket || tc.responseLen == 780 {
					attempts = 2
				}
				for attempt := 0; attempt < attempts; attempt++ {
					if attempt > 0 && (tc.responseLen == 780) {
						payload = []byte(strings.ReplaceAll(string(payload), "turn-1", "new-turn"))
					}
					hook.Reset()
					recorder := httptest.NewRecorder()
					engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
					if recorder.Code != http.StatusOK {
						t.Fatalf("HTTP status = %d", recorder.Code)
					}
					wireState := <-captured
					wireLen := len(wireState)
					wantLen := tc.passiveLen
					if attempt > 0 && tc.responseLen > 0 {
						wantLen = tc.responseLen
					}
					if tc.ticketLen > 0 {
						wantLen = tc.ticketLen
					}
					if attempt > 0 && tc.responseLen == 780 && wireState != responseState {
						t.Fatal("request did not use the new response ticket")
					}
					if wireLen != wantLen {
						t.Fatalf("outbound length = %d, want %d", wireLen, wantLen)
					}
					record := capture.await(t)
					if record.ResponseModel != servedModel {
						t.Errorf("usage response model = %q, want %q", record.ResponseModel, servedModel)
					}
					if record.RequestTurnStateLen != wireLen || record.ResponseTurnStateLen != tc.responseLen {
						t.Errorf("usage lengths = %d/%d, want %d/%d", record.RequestTurnStateLen, record.ResponseTurnStateLen, wireLen, tc.responseLen)
					}
					wantPair := fmt.Sprintf("| %d/%d |", wireLen, tc.responseLen)
					found := false
					for _, entry := range hook.AllEntries() {
						if strings.Contains(entry.Message, `"/v1/responses"`) && strings.Contains(entry.Message, wantPair) {
							found = true
							if !strings.Contains(entry.Message, " | "+tc.model+"/"+servedModel+" | ") {
								t.Errorf("access log lost response model: %s", entry.Message)
							}
						}
					}
					if !found {
						t.Errorf("access log missing final lengths %s (attempt %d)", wantPair, attempt)
					}
				}
				if isWebsocket && handshakes.Load() != 1 {
					t.Fatalf("websocket handshakes = %d, want one reused connection", handshakes.Load())
				}
			})
		}
	}
}
