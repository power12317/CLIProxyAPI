package executor

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
)

type ticketBorrowRoundTrip func(*http.Request) (*http.Response, error)

func (f ticketBorrowRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }
func executorBorrowState(now time.Time, nonce byte) string {
	raw := make([]byte, 585)
	raw[0] = 128
	binary.BigEndian.PutUint64(raw[1:9], uint64(now.Unix()))
	raw[9] = nonce
	return base64.URLEncoding.EncodeToString(raw)
}

func TestCodexSSEBorrowOverridesSameTurnWithoutPersistingTarget(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "execute", true: "execute-stream"}[stream], func(t *testing.T) {
			now := time.Now().Truncate(time.Second)
			sourceState := executorBorrowState(now.Add(-50*time.Minute), 1)
			route := helps.CodexTicketRoute{CFLB: helps.CodexRouteCookie{Value: "donor-cf"}, OaiLB: helps.CodexRouteCookie{Value: "donor-oai"}}
			var fetches, shots, business atomic.Int32
			var unavailable atomic.Bool
			source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				fetches.Add(1)
				if unavailable.Load() {
					w.WriteHeader(503)
					return
				}
				var input map[string]string
				_ = json.NewDecoder(r.Body).Decode(&input)
				if input["auth_id"] != "donor" || input["model"] != "gpt-5.6-sol" {
					t.Error("source did not use final overridden model")
				}
				_ = json.NewEncoder(w).Encode(helps.NewCodexOaiLBBorrowResponse(helps.CodexTurnStateTicket{AccountID: "donor", Model: input["model"], State: sourceState, Length: 780, CapturedAt: now.Add(-50 * time.Minute), ExpiresAt: now.Add(10 * time.Minute), Route: &route}, time.Now()))
			}))
			defer source.Close()
			cfg, err := config.ParseConfigBytes([]byte(`codex:
  turn-state-ticket:
    enabled: true
payload:
  override:
    - models:
        - name: gpt-6-astra
          protocol: codex
      params:
        model: gpt-5.6-sol
        instructions: final-business-instructions
  filter:
    - models:
        - name: gpt-6-astra
          protocol: codex
      params:
        - client_metadata.x-codex-turn-state
`))
			if err != nil {
				t.Fatal(err)
			}
			cfg.CodexHeaderDefaults.OaiLBBorrow = &config.CodexOaiLBBorrowConfig{SourceURL: source.URL, SourceManagementKey: "key", SourceAuthID: "donor"}
			auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "target-token", "account_id": "target-account"}}
			helps.StoreCodexTurnStateTicket(auth, helps.CodexTurnStateTicket{Model: "gpt-6-astra", State: strings.Repeat("local", 156), ExpiresAt: now.Add(time.Hour)})
			t.Cleanup(func() { helps.InvalidateCodexCookieJar(auth.ID) })
			targetState := ""
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", ticketBorrowRoundTrip(func(r *http.Request) (*http.Response, error) {
				raw, _ := io.ReadAll(r.Body)
				if r.Header.Get("Content-Encoding") == "zstd" {
					t.Fatal("test unexpectedly used official encoding")
				}
				if gjson.GetBytes(raw, "model").String() != "gpt-5.6-sol" {
					t.Error("wrong final upstream model")
				}
				if r.Header.Get("Authorization") != "Bearer target-token" || r.Header.Get("Chatgpt-Account-Id") != "target-account" {
					t.Error("donor identity sent upstream")
				}
				for _, name := range []string{"__cflb", "__oailb"} {
					c, e := r.Cookie(name)
					if e != nil || (name == "__cflb" && c.Value != "donor-cf") || (name == "__oailb" && c.Value != "donor-oai") {
						t.Error("wrong route pair")
					}
				}
				header := make(http.Header)
				header.Set("Content-Type", "text/event-stream")
				if gjson.GetBytes(raw, "input.0.content.0.text").String() == "ping" {
					n := shots.Add(1)
					if n%2 == 1 {
						if r.Header.Get(helps.CodexTurnStateTicketHeader) != sourceState {
							t.Error("renewal did not reuse donor seed")
						}
						targetState = executorBorrowState(time.Now(), byte(n+1))
						header.Set(helps.CodexTurnStateTicketHeader, targetState)
					} else if r.Header.Get(helps.CodexTurnStateTicketHeader) != targetState {
						t.Error("continuation did not use target ticket")
					}
				} else {
					n := business.Add(1)
					if r.Header.Get(helps.CodexTurnStateTicketHeader) != targetState || gjson.GetBytes(raw, "client_metadata.x-codex-turn-state").Exists() {
						t.Error("old turn state or local hour cache bypassed borrowing")
					}
					if gjson.GetBytes(raw, "instructions").String() != "final-business-instructions" {
						t.Error("final payload overwritten")
					}
					if n == 1 || n == 3 {
						header.Set(helps.CodexTurnStateTicketHeader, executorBorrowState(time.Now(), 99))
					}
				}
				complete := `data: {"type":"response.completed","response":{"id":"resp_borrow","status":"completed","model":"gpt-5.6-sol","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}` + "\n\n"
				return &http.Response{StatusCode: 200, Header: header, Body: io.NopCloser(strings.NewReader(complete)), Request: r}, nil
			}))
			executor := NewCodexExecutor(cfg)
			req := cliproxyexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","instructions":"original","input":[{"role":"user","content":[{"type":"input_text","text":"business"}]}],"client_metadata":{"x-codex-turn-state":"inbound-old"}}`)}
			opts := cliproxyexecutor.Options{SourceFormat: sdktranslator.FormatCodex, Headers: http.Header{"X-Codex-Turn-Metadata": []string{`{"turn_id":"same-turn","session_id":"same-session"}`}, "X-Codex-Turn-State": []string{"passive-old"}}}
			run := func() error {
				if stream {
					result, err := executor.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						return err
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
					return nil
				}
				_, err := executor.Execute(ctx, auth, req, opts)
				return err
			}
			for range 3 {
				if err := run(); err != nil {
					t.Fatal(err)
				}
			}
			if fetches.Load() != 2 || shots.Load() != 4 || business.Load() != 3 {
				t.Fatalf("wrong borrow reuse sequence: fetch=%d probe=%d business=%d", fetches.Load(), shots.Load(), business.Load())
			}
			original, _ := json.Marshal(auth.Metadata)
			if strings.Contains(string(original), targetState) {
				t.Fatal("borrowed target persisted in one-hour cache")
			}
			unavailable.Store(true)
			if err := run(); err == nil || business.Load() != 3 {
				t.Fatal("failed borrow silently fell back to a local/passive ticket")
			}
		})
	}
}
