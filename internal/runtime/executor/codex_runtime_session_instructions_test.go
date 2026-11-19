package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/MoeForge/moego/ojson"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/kvstore"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestRuntimeSessionInstructionsIPC(t *testing.T) {
	const baseline = "first instructions"
	for _, capability := range []string{"full", "websocket", ""} {
		t.Run("capability-"+capability, func(t *testing.T) {
			captured := make(chan bridge.Request, 1)
			w, a := runtimeTestMaster(t, "one", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
				captured <- r
				runtimeTestAccept(c, m, r)
				runtimeTestFinish(c, r, `{"type":"response.created","response":{"id":"resp_test"}}`, runtimeTerminal)
			}, func(c *bridge.Capabilities) {
				c.SessionInstructions = capability
				c.ResponsesWebsocket = capability == "websocket"
			})
			exec := runtimeTestExecutor(w)
			exec.cfg.Codex.ForceWebsocket = capability == "websocket"
			opts := runtimeTestOptions()
			opts.Headers = http.Header{"Session-Id": {t.Name()}}
			call := func(raw string, status int) bridge.Request {
				t.Helper()
				_, err := exec.Execute(t.Context(), a, core.Request{Model: "runtime-model", Payload: []byte(raw)}, opts)
				if status == 200 {
					if err != nil {
						t.Fatal(err)
					}
					return <-captured
				}
				var e interface{ StatusCode() int }
				if !errors.As(err, &e) || e.StatusCode() != status {
					t.Fatalf("want %d, got %v", status, err)
				}
				select {
				case <-captured:
					t.Fatal("rejected request reached worker inference")
				default:
				}
				return bridge.Request{}
			}
			start := `{"input":"hello","instructions":"first instructions","client_metadata":{"cpa_session_instructions":"start","keep":"yes"}}`
			if capability == "" {
				call(start, 503)
				if plain := call(`{"input":"hi"}`, 200); plain.SessionInstructions != nil {
					t.Fatal("ordinary request received a managed control")
				}
				return
			}
			first := call(start, 200)
			control := first.SessionInstructions
			if control == nil || control.Mode != "start" || control.HTTPOnly != (capability != "websocket") || control.Scope == "" || control.BaselineDigest != kvstore.HashKeyPart(baseline) {
				t.Fatalf("invalid start control: %+v", control)
			}
			body := ojson.NewDoc(first.Request)
			if body.Get("input.0.role").String() != "developer" || body.Get("input.0.content.0.text").String() != baseline || body.Get("instructions").String() != "" || body.Get("input.#").Int() != 2 || body.Get("client_metadata.cpa_session_instructions").Exists() || body.Get("client_metadata.keep").String() != "yes" {
				t.Fatalf("invalid full request: %s", first.Request)
			}
			delta := `{"input":[],"previous_response_id":"resp_test","client_metadata":{"cpa_session_instructions":"reuse"}}`
			if capability == "full" {
				call(delta, 409)
			} else {
				exec.cfg.Codex.ForceWebsocket = false
				call(delta, 409)
				exec.cfg.Codex.ForceWebsocket = true
				a = runtimeTestAuth("other-worker")
				call(delta, 409)
				a = runtimeTestAuth("one")
				call(strings.Replace(delta, "resp_test", "other-parent", 1), 409)
				next := call(delta, 200)
				if next.SessionInstructions.PreviousResponseID != "resp_test" || next.SessionInstructions.Scope != control.Scope || strings.Contains(string(next.Request), baseline) {
					t.Fatalf("invalid delta: %+v %s", next.SessionInstructions, next.Request)
				}
			}
			// Complete history may use another credential. The baseline follows the caller,
			// while the worker execution session and connection ownership remain separate.
			a = runtimeTestAuth("two")
			exec.cfg.Payload.Override = []config.PayloadRule{{Models: []config.PayloadModelRule{{Name: "*"}}, Params: map[string]any{"input.0.content.0.text": "configured"}}}
			another := call(`{"input":"hi","instructions":"this turn","client_metadata":{"cpa_session_instructions":"reuse"}}`, 200)
			if another.SessionID == first.SessionID || another.SessionInstructions.Scope != control.Scope || another.SessionInstructions.BaselineDigest != "" || ojson.GetBytes(another.Request, "input.0.content.0.text").String() != "configured" || ojson.GetBytes(another.Request, "instructions").String() != "this turn" {
				t.Fatalf("invalid configured reuse: %+v %s", another.SessionInstructions, another.Request)
			}
			opts.Headers.Set("Session-Id", t.Name()+"-another")
			call(`{"input":[],"client_metadata":{"cpa_session_instructions":"reuse"}}`, 409)
			opts.Headers = nil
			call(start, 400)
		})
	}
}

func managedClientTestServer(t *testing.T, cfg *config.Config, auth *coreauth.Auth) *httptest.Server {
	t.Helper()
	const model = "gpt-6.1-sol"
	manager := coreauth.NewManager(nil, nil, nil)
	manager.SetConfig(cfg)
	manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: model}})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
	h := openai.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
	router := gin.New()
	router.Use(func(c *gin.Context) { c.Set("userApiKey", "managed-session-test-caller"); c.Next() })
	router.GET("/v1/responses", h.ResponsesWebsocket)
	router.POST("/v1/responses", h.Responses)
	server := httptest.NewServer(router)
	t.Cleanup(server.Close)
	return server
}

func testManagedClientSSE(t *testing.T, cfg *config.Config, auth *coreauth.Auth, readHTTP func() []byte) {
	t.Helper()
	server := managedClientTestServer(t, cfg, auth)
	post := func(body string, status int) []byte {
		t.Helper()
		ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/responses", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Session-Id", t.Name())
		req.Header.Set("Content-Type", "application/json")
		resp, err := server.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = resp.Body.Close() }()
		data, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != status {
			t.Fatalf("HTTP status=%d body=%s err=%v", resp.StatusCode, data, err)
		}
		if status == 200 && (!strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") || !bytes.Contains(data, []byte("response.completed"))) {
			t.Fatalf("incomplete SSE: %s", data)
		}
		return data
	}
	post(`{"model":"gpt-6.1-sol","stream":true,"instructions":"http baseline","input":"one","client_metadata":{"cpa_session_instructions":"start"}}`, 200)
	if body := readHTTP(); !bytes.Contains(body, []byte("http baseline")) || bytes.Contains(body, []byte("cpa_session_instructions")) {
		t.Fatalf("start: %s", body)
	}
	failure := post(`{"model":"gpt-6.1-sol","stream":true,"previous_response_id":"other-worker","input":"delta","client_metadata":{"cpa_session_instructions":"reuse"}}`, 409)
	if !bytes.Contains(failure, []byte("previous_response_not_found")) {
		t.Fatalf("unexpected parent error: %s", failure)
	}
	post(`{"model":"gpt-6.1-sol","stream":true,"input":"complete new history","client_metadata":{"cpa_session_instructions":"reuse"}}`, 200)
	if body := readHTTP(); !bytes.Contains(body, []byte("http baseline")) || !bytes.Contains(body, []byte("complete new history")) || ojson.GetBytes(body, "previous_response_id").Exists() {
		t.Fatalf("full HTTP recovery: %s", body)
	}
}

// Exercise the public WebSocket handler, auth selection, payload translation and
// runtime IPC together. The real-worker suites reuse the same client sequence.
func testManagedClientWebsocket(t *testing.T, cfg *config.Config, auth *coreauth.Auth, incremental bool, readRequest func() []byte) {
	t.Helper()
	server := managedClientTestServer(t, cfg, auth)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	c, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Session-Id": {t.Name()}})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = c.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = c.Close() })
	defer stop()
	turn := func(body string) *ojson.Doc {
		t.Helper()
		if errWrite := c.WriteMessage(websocket.TextMessage, []byte(body)); errWrite != nil {
			t.Fatal(errWrite)
		}
		for {
			_, payload, errRead := c.ReadMessage()
			if errRead != nil {
				t.Fatal(errRead)
			}
			event := ojson.NewDoc(payload)
			switch event.Get("type").String() {
			case "error", "response.failed", "response.completed":
				return event
			}
		}
	}
	start := turn(`{"type":"response.create","model":"gpt-6.1-sol","instructions":"baseline for the session","client_metadata":{"cpa_session_instructions":"start"},"input":[{"type":"message","role":"user","content":"native-continuation-step-one"}]}`)
	parent := start.Get("response.id").String()
	if start.Get("type").String() != "response.completed" || parent == "" {
		t.Fatalf("start: %s", start.String())
	}
	first := readRequest()
	if !bytes.Contains(first, []byte("baseline for the session")) || bytes.Contains(first, []byte("cpa_session_instructions")) {
		t.Fatalf("start wire: %s", first)
	}
	next := ojson.NewDocString(`{"type":"response.create","input":[{"type":"message","role":"user","content":"native-continuation-step-two"}]}`)
	_ = next.Set("previous_response_id", parent)
	response := turn(next.String())
	if incremental {
		if response.Get("type").String() != "response.completed" {
			t.Fatalf("delta: %s", response.String())
		}
		wire := readRequest()
		if ojson.GetBytes(wire, "previous_response_id").String() != parent || bytes.Contains(wire, []byte("native-continuation-step-one")) || bytes.Contains(wire, []byte("baseline for the session")) {
			t.Fatalf("delta wire: %s", wire)
		}
	} else if response.Get("error.code").String() != "previous_response_not_found" {
		t.Fatalf("expected a request-scoped rejection: %s", response.String())
	}
	recovery := turn(`{"type":"response.create","model":"gpt-6.1-sol","client_metadata":{"cpa_session_instructions":"reuse"},"input":[{"type":"message","role":"user","content":"native-continuation-step-three"}]}`)
	if recovery.Get("type").String() != "response.completed" {
		t.Fatalf("full recovery: %s", recovery.String())
	}
	wire := readRequest()
	if ojson.GetBytes(wire, "previous_response_id").Exists() || !bytes.Contains(wire, []byte("baseline for the session")) {
		t.Fatalf("recovery wire: %s", wire)
	}
}

func TestRuntimeManagedClientWebsocket(t *testing.T) {
	for _, capability := range []string{"full", "websocket"} {
		t.Run(capability, func(t *testing.T) {
			captured := make(chan []byte, 4)
			var sequence atomic.Int32
			w, a := runtimeTestMaster(t, "managed-wire.json", func(c *websocket.Conn, m runtimeTestRPC, r bridge.Request) {
				captured <- r.Request
				runtimeTestAccept(c, m, r)
				id := fmt.Sprintf("resp_managed_%d", sequence.Add(1))
				runtimeTestFinish(c, r, `{"type":"response.created","response":{"id":"`+id+`"}}`, strings.ReplaceAll(runtimeTerminal, "resp_test", id))
			}, func(c *bridge.Capabilities) {
				c.SessionInstructions = capability
				c.ResponsesWebsocket = capability == "websocket"
			})
			cfg := runtimeTestExecutor(w).cfg
			runtimeImageAuth(t, cfg, a)
			t.Run("ws", func(t *testing.T) {
				testManagedClientWebsocket(t, cfg, a, capability == "websocket", func() []byte { return <-captured })
			})
			t.Run("sse", func(t *testing.T) { testManagedClientSSE(t, cfg, a, func() []byte { return <-captured }) })
		})
	}
}
