package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func runtimeSourceAuth(id string) *coreauth.Auth {
	a := ticketProbeTestAuth(id, "same@example.invalid", "same-account")
	a.Metadata["codex_cli"] = map[string]any{"enabled": true}
	return a
}

func runtimeSourceConfig() *config.Config {
	return &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}, TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true, Models: []string{"gpt-6-astra"}}}}
}

func runtimeSourceRequest(model, cookies string) bridge.UpstreamLog {
	return bridge.UpstreamLog{Kind: "request", Method: "POST", URL: codexTurnStateTicketProbeURL,
		Headers: http.Header{"Cookie": {cookies}}, Body: `{"model":"` + model + `"}`}
}

func TestCodexRuntimeSourceUsesExistingResponses(t *testing.T) {
	for _, kind := range []string{"header", "response.metadata", "codex.response.metadata"} {
		t.Run(kind, func(t *testing.T) {
			auth, cfg := runtimeSourceAuth(t.Name()), runtimeSourceConfig()
			state := borrowTestState(time.Now().Add(-50*time.Minute), 1)
			logger := NewCodexRuntimeLog(t.Context(), cfg, auth, "public-alias")
			logger.CaptureSourceTickets([]byte(`{"model":"final-model"}`))
			logger.Record(runtimeSourceRequest("gpt-6-astra", "__cflb=sent-cf; __oailb=sent-oai"))
			headers := http.Header{"Set-Cookie": {"__oailb=response-oai; Max-Age=3600"}}
			if kind == "header" {
				headers.Set(CodexTurnStateTicketHeader, state)
			}
			logger.Record(bridge.UpstreamLog{Kind: "response", StatusCode: 200, Headers: headers})
			if kind != "header" {
				event, _ := json.Marshal(map[string]any{"type": kind, "headers": map[string]string{"x-codex-turn-state": state}})
				logger.Event(event)
			}
			ticket, err := ExportCodexSourceTicket(auth, "gpt-6-astra", time.Now())
			if err != nil || ticket.State != state || ticket.Route.CFLB.Value != "sent-cf" || ticket.Route.OaiLB.Value != "response-oai" {
				t.Fatalf("existing exchange did not produce a paired source: %v", err)
			}
			if _, err = ExportCodexSourceTicket(auth, "public-alias", time.Now()); err == nil {
				t.Fatal("snapshot stored under the public alias")
			}
			status := CodexTurnStateTicketStatuses(auth, cfg.Codex.EffectiveTurnStateTicket(), time.Now())
			if len(status) != 1 || !status[0].Ready || status[0].RemainingSeconds > 600 {
				t.Fatalf("runtime source is not visible or got a sliding lifetime: %+v", status)
			}
			borrowCfg := cfg.CloneForRuntime()
			borrowCfg.CodexHeaderDefaults.OaiLBBorrow = &config.CodexOaiLBBorrowConfig{}
			if CodexCookieJarForAuth(auth) != nil || CodexTicketBorrowEnabled(borrowCfg, auth) {
				t.Fatal("source eligibility enabled a runtime borrower or local cookie jar")
			}
			h := http.Header{CodexTurnStateTicketHeader: {"native-ticket"}}
			if err := ApplyCodexTurnStateTicket(auth, cfg.Codex.EffectiveTurnStateTicket(), "gpt-6-astra", h); err != nil || h.Get(CodexTurnStateTicketHeader) != "native-ticket" {
				t.Fatal("CPA injected its source ticket into runtime headers")
			}
			body := []byte(`{"client_metadata":{"x-codex-turn-state":"native-ticket"}}`)
			if string(ApplyCodexTurnStateTicketBody(auth, cfg.Codex.EffectiveTurnStateTicket(), "gpt-6-astra", body)) != string(body) {
				t.Fatal("CPA injected a ticket into the runtime body")
			}
		})
	}
}

func TestCodexRuntimeSourcePairingAndPolicy(t *testing.T) {
	for _, mode := range []string{"disabled", "borrower", "excluded-model", "redacted", "deleted", "retry", "plain-output", "malformed", "expired"} {
		t.Run(mode, func(t *testing.T) {
			auth, cfg := runtimeSourceAuth(t.Name()), runtimeSourceConfig()
			state := borrowTestState(time.Now(), 2)
			switch mode {
			case "malformed":
				state = strings.Repeat("x", 780)
			case "expired":
				state = borrowTestState(time.Now().Add(-2*time.Hour), 2)
			case "disabled":
				cfg.Codex.TurnStateTicket.Enabled = false
			case "borrower":
				cfg.CodexHeaderDefaults.OaiLBBorrow = &config.CodexOaiLBBorrowConfig{}
			case "excluded-model":
				cacheAll := false
				cfg.Codex.TurnStateTicket.CacheAllModels = &cacheAll
				cfg.Codex.TurnStateTicket.Models = []string{"another-model"}
			}
			logger := NewCodexRuntimeLog(t.Context(), cfg, auth, "gpt-6-astra")
			logger.CaptureSourceTickets([]byte(`{"model":"gpt-6-astra"}`))
			cookies := "__cflb=old-cf; __oailb=old-oai"
			if mode == "redacted" {
				cookies = "[REDACTED]"
			}
			logger.Record(runtimeSourceRequest("gpt-6-astra", cookies))
			if mode == "retry" {
				logger.Record(bridge.UpstreamLog{Kind: "response", StatusCode: 401, Headers: http.Header{"Set-Cookie": {"__oailb=first-attempt"}}})
				logger.Record(runtimeSourceRequest("gpt-6-astra", "__cflb=next-cf"))
			}
			h := http.Header{CodexTurnStateTicketHeader: {state}}
			if mode == "deleted" {
				h.Add("Set-Cookie", "__oailb=; Max-Age=0")
			}
			if mode == "plain-output" {
				h.Del(CodexTurnStateTicketHeader)
			}
			logger.Record(bridge.UpstreamLog{Kind: "response", StatusCode: 200, Headers: h})
			if mode == "plain-output" {
				event, _ := json.Marshal(map[string]any{"type": "response.output_text.delta", "headers": map[string]string{"x-codex-turn-state": state}})
				logger.Event(event)
			}
			if _, err := ExportCodexSourceTicket(auth, "gpt-6-astra", time.Now()); err == nil {
				t.Fatal("ineligible or unpaired material was exported")
			}
		})
	}
}

func TestCodexRuntimeSourceIsolationAndOriginalDeadlines(t *testing.T) {

	cfg := runtimeSourceConfig()
	a, b := runtimeSourceAuth("mac"), runtimeSourceAuth("windows")
	la, lb := NewCodexRuntimeLog(t.Context(), cfg, a, "gpt-6-astra"), NewCodexRuntimeLog(t.Context(), cfg, b, "gpt-6-astra")
	la.CaptureSourceTickets([]byte(`{"model":"gpt-6-astra"}`))
	lb.CaptureSourceTickets([]byte(`{"model":"gpt-6-astra"}`))
	la.Record(runtimeSourceRequest("gpt-6-astra", "__cflb=mac-cf; __oailb=mac-oai"))
	lb.Record(runtimeSourceRequest("gpt-6-astra", "__cflb=win-cf; __oailb=win-oai"))
	now := time.Now().Truncate(time.Second)
	la.ticket.now = func() time.Time { return now }
	lb.ticket.now = func() time.Time { return now }
	stateA, stateB := borrowTestState(now, 1), borrowTestState(now, 2)
	lb.Record(bridge.UpstreamLog{Kind: "response", StatusCode: 200, Headers: http.Header{CodexTurnStateTicketHeader: {stateB}}})
	la.Record(bridge.UpstreamLog{Kind: "response", StatusCode: 200, Headers: http.Header{CodexTurnStateTicketHeader: {stateA}, "Set-Cookie": {"__oailb=mac-oai; Max-Age=1200"}}})
	ta, err := ExportCodexSourceTicket(a, "gpt-6-astra", now)
	if err != nil || ta.State != stateA || ta.Route.CFLB.Value != "mac-cf" || !ta.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatal("mac source was mixed or renewed", err)
	}
	tb, err := ExportCodexSourceTicket(b, "gpt-6-astra", now)
	if err != nil || tb.State != stateB || tb.Route.CFLB.Value != "win-cf" {
		t.Fatal("windows source was mixed", err)
	}
	la.ticket.now = func() time.Time { return now.Add(10 * time.Minute) }
	event, _ := json.Marshal(map[string]any{"type": "response.metadata", "headers": map[string]string{"x-codex-turn-state": stateA}})
	la.Event(event)
	if got := codexTurnStateTicketForAuth(a, "gpt-6-astra"); !got.Route.OaiLB.ExpiresAt.Equal(now.Add(20*time.Minute)) || !got.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatal("metadata reception extended cookie or ticket deadlines")
	}
	if status := CodexTurnStateTicketStatuses(a, cfg.Codex.EffectiveTurnStateTicket(), now.Add(21*time.Minute)); status[0].Ready || status[0].Blocked {
		t.Fatal("expired runtime source was shown as usable or blocked business inference")
	}
}

// The fixture deliberately advertises only v3, and rejects any added RPC field.
func runtimeSourceMaster(t *testing.T, calls *atomic.Int32, metadata bool) string {
	t.Helper()
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			var m struct {
				ID     json.RawMessage
				Method string
				Params json.RawMessage
			}
			if conn.ReadJSON(&m) != nil {
				return
			}
			switch m.Method {
			case "initialize":
				_ = conn.WriteJSON(map[string]any{"id": m.ID, "result": map[string]any{}})
			case "initialized":
			case "cpa/capabilities/read":
				_ = conn.WriteJSON(map[string]any{"id": m.ID, "result": bridge.Capabilities{ProtocolVersion: 3, ExecutionMode: "inference-only", RawBody: true, Operations: []string{"responses"}}})
			case "cpa/inference/start":
				var req bridge.Request
				_ = json.Unmarshal(m.Params, &req)
				gjson.ParseBytes(m.Params).ForEach(func(key, _ gjson.Result) bool {
					switch key.String() {
					case "requestId", "credentialId", "operation", "sourceFormat", "sessionId", "request":
					default:
						t.Errorf("added IPC field %s", key.String())
					}
					return true
				})
				if !gjson.GetBytes(req.Request, "stream").Bool() || gjson.GetBytes(req.Request, "instructions").String() != "final-payload" || gjson.GetBytes(req.Request, "client_metadata").Exists() {
					t.Error("runtime probe overwrote the final payload rules")
				}
				n := calls.Add(1)
				send := func(method string, params any) {
					_ = conn.WriteJSON(map[string]any{"method": method, "params": params})
				}
				request := runtimeSourceRequest("gpt-6-astra", "__cflb=runtime-cf; __oailb=runtime-oai")
				request.RequestID = req.RequestID
				send("cpa/inference/upstream", request)
				state := borrowTestState(time.Now(), byte(n))
				h := http.Header{}
				if !metadata {
					h.Set(CodexTurnStateTicketHeader, state)
				}
				send("cpa/inference/upstream", bridge.UpstreamLog{RequestID: req.RequestID, Kind: "response", StatusCode: 200, Headers: h})
				_ = conn.WriteJSON(map[string]any{"id": m.ID, "result": map[string]any{"requestId": req.RequestID, "statusCode": 200}})
				if metadata {
					event, _ := json.Marshal(map[string]any{"type": "response.metadata", "headers": map[string]string{"x-codex-turn-state": state}})
					send("cpa/inference/body", map[string]any{"requestId": req.RequestID, "bodyBase64": []byte("data: " + string(event) + "\n\n")})
				}
				send("cpa/inference/completed", map[string]any{"requestId": req.RequestID})
				return
			}
		}
	}))
	t.Cleanup(s.Close)
	return "ws" + strings.TrimPrefix(s.URL, "http")
}

func TestCodexRuntimeHarvesterUsesV3AndExistingRenewal(t *testing.T) {
	for _, metadata := range []bool{false, true} {
		t.Run(map[bool]string{false: "header", true: "metadata"}[metadata], func(t *testing.T) {
			var calls atomic.Int32
			cfg, err := config.ParseConfigBytes([]byte(`codex:
  turn-state-ticket:
    enabled: true
    models: [gpt-6-astra]
payload:
  override:
    - models: [{name: gpt-6-astra, protocol: codex}]
      params: {instructions: final-payload}
  filter:
    - models: [{name: gpt-6-astra, protocol: codex}]
      params: [client_metadata]
`))
			if err != nil {
				t.Fatal(err)
			}
			cfg.Codex.Runtime = config.CodexRuntimeConfig{Enabled: true, URL: runtimeSourceMaster(t, &calls, metadata)}
			cfg.Codex.TurnStateTicket.HarvestProxyURL = "invalid://must-not-affect-runtime"
			manager := coreauth.NewManager(nil, nil, nil)
			auth, err := manager.Register(t.Context(), runtimeSourceAuth("runtime-source"))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { InvalidateCodexTurnStateTickets(auth.ID) })
			h := NewCodexTurnStateTicketHarvester(CodexTurnStateTicketHarvesterOptions{Config: func() *config.Config { return cfg }, List: manager.List, Update: func(ctx context.Context, base, updated *coreauth.Auth) error {
				t.Error("runtime ticket must not update the credential store")
				return nil
			}})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", codexRoundTripFunc(func(*http.Request) (*http.Response, error) {
				t.Error("runtime harvester attempted direct HTTP")
				return nil, io.ErrUnexpectedEOF
			}))
			h.refresh(ctx)
			h.refresh(ctx)
			current, _ := manager.GetByID(auth.ID)
			if _, err := ExportCodexSourceTicket(current, "gpt-6-astra", time.Now()); err != nil || calls.Load() != 1 {
				t.Fatalf("runtime source did not persist or fresh source was reprobed: %v, calls=%d", err, calls.Load())
			}
			ticket := codexTurnStateTicketForAuth(current, "gpt-6-astra")
			ticket.ExpiresAt = time.Now().Add(9 * time.Minute)
			StoreCodexTurnStateTicket(current, *ticket)
			_, _ = manager.Update(t.Context(), current)
			h.refresh(ctx)
			if calls.Load() != 2 {
				t.Fatal("ten-minute renewal did not use runtime")
			}
			cfg.CodexHeaderDefaults.OaiLBBorrow = &config.CodexOaiLBBorrowConfig{}
			h.refresh(ctx)
			if calls.Load() != 2 {
				t.Fatal("borrower started a background source probe")
			}
		})
	}
}
