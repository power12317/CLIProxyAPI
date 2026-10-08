package codexruntime

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gorilla/websocket"
)

func serveMaster(t *testing.T, caps Capabilities, handle func(*websocket.Conn, message)) string {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Error("master connection must not send a bridge token")
		}
		upgrader := websocket.Upgrader{}
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer func() { _ = conn.Close() }()
		for {
			var m message
			if errRead := conn.ReadJSON(&m); errRead != nil {
				return
			}
			switch m.Method {
			case "initialize":
				_ = conn.WriteJSON(map[string]any{"id": m.ID, "result": map[string]any{}})
			case "initialized":
			case "cpa/capabilities/read":
				_ = conn.WriteJSON(map[string]any{"id": m.ID, "result": caps})
			default:
				handle(conn, m)
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	return "ws" + strings.TrimPrefix(server.URL, "http") + "/cpa/v1/ws"
}

func validCaps() Capabilities {
	return Capabilities{
		ProtocolVersion: ProtocolVersion, RuntimeVersion: "test", UpstreamRevision: "test-sha",
		ExecutionMode: "inference-only", RawBody: true, Operations: []string{"responses"},
		ManualOAuth: true, UpstreamLogs: true, UpstreamBodyLogs: true,
	}
}

func request() Request {
	return Request{RequestID: "req", CredentialID: "Group/Account-A.json", Operation: "responses", SourceFormat: "openai-response", SessionID: "session", Request: json.RawMessage(`{"model":"test","input":[]}`)}
}

func accept(conn *websocket.Conn, m message) {
	_ = conn.WriteJSON(map[string]any{"id": m.ID, "result": map[string]any{
		"requestId": "req", "statusCode": 201,
		"headers": http.Header{"set-cookie": {"private"}, "content-type": {"text/event-stream"}, "x-request-id": {"upstream"}, "authorization": {"private"}},
	}})
}

func notify(conn *websocket.Conn, method string, params any) {
	_ = conn.WriteJSON(map[string]any{"method": method, "params": params})
}

func sendBody(conn *websocket.Conn, body []byte) {
	notify(conn, "cpa/inference/body", map[string]any{"requestId": "req", "bodyBase64": body})
}

func dialMaster(t *testing.T, endpoint string) *Client {
	t.Helper()
	client, err := Dial(context.Background(), endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}

func TestMasterPreservesCredentialIDsWithoutSeparateAccountIdentity(t *testing.T) {
	seen := make(chan map[string]json.RawMessage, 2)
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, m message) {
		if m.Method != "cpa/inference/start" {
			t.Errorf("unexpected method %q", m.Method)
			return
		}
		var params map[string]json.RawMessage
		if err := json.Unmarshal(m.Params, &params); err != nil {
			t.Error(err)
			return
		}
		seen <- params
		accept(conn, m)
		notify(conn, "cpa/inference/completed", map[string]string{"requestId": "req"})
	})
	for _, credentialID := range []string{"Group/Account-A.json", "group/account-a.json"} {
		client := dialMaster(t, endpoint)
		req := request()
		req.CredentialID = credentialID
		if err := client.Start(req); err != nil {
			t.Fatal(err)
		}
		params := <-seen
		var got string
		if err := json.Unmarshal(params["credentialId"], &got); err != nil || got != credentialID {
			t.Fatalf("credential ID changed: %q (%v)", got, err)
		}
		for _, name := range []string{"accountId", "workerId", "owner"} {
			if _, exists := params[name]; exists {
				t.Fatalf("request has obsolete identity %s", name)
			}
		}
		if _, err := client.Next(); err != io.EOF {
			t.Fatalf("empty HTTP EOF = %v", err)
		}
	}
}

func TestRawBodyFidelityAndOrderedLogsBeforeAcceptance(t *testing.T) {
	raw := []byte(": keepalive\r\nid: 9\r\nevent: response.future_extension\r\ndata: {\"delta\":\"你好\",\"unknown\":[1,true]}\r\n\r\n: no-terminal-event")
	split := bytes.Index(raw, []byte("你好")) + 1
	chunks := [][]byte{raw[:split], raw[split : split+2], raw[split+2:]}
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, m message) {
		notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "request", URL: "https://chatgpt.com/backend-api/codex/responses", Method: "POST", Headers: http.Header{"x-source": {"actual"}}, Body: `{"input":[]}`})
		notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "response", StatusCode: 201, Headers: http.Header{"x-request-id": {"upstream"}}})
		sendBody(conn, chunks[0])
		// A later log must remain behind the body that preceded it.
		notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "error", Message: "diagnostic"})
		accept(conn, m)
		for _, chunk := range chunks[1:] {
			sendBody(conn, chunk)
		}
		notify(conn, "cpa/inference/completed", map[string]string{"requestId": "req"})
	})
	client := dialMaster(t, endpoint)
	var logs []UpstreamLog
	client.OnUpstream = func(entry UpstreamLog) { logs = append(logs, entry) }
	if err := client.Start(request()); err != nil {
		t.Fatal(err)
	}
	if client.StatusCode != 201 || client.Headers.Get("X-Request-ID") != "upstream" || client.Headers.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("response metadata changed: status=%d headers=%v", client.StatusCode, client.Headers)
	}
	if client.Headers.Get("Set-Cookie") != "" || client.Headers.Get("Authorization") != "" {
		t.Fatal("private headers exposed", client.Headers)
	}
	var got []byte
	for _, want := range chunks {
		frame, err := client.Next()
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Equal(frame.Body, want) {
			t.Fatalf("chunk changed: got %q, want %q", frame.Body, want)
		}
		got = append(got, frame.Body...)
	}
	if _, err := client.Next(); err != io.EOF {
		t.Fatalf("completed without parsing a model terminal = %v", err)
	}
	if !bytes.Equal(got, raw) {
		t.Fatal("raw SSE lost content")
	}
	var kinds []string
	var logged []byte
	for _, entry := range logs {
		kinds = append(kinds, entry.Kind)
		if entry.Kind == "body" {
			logged = append(logged, entry.BodyBytes...)
		}
	}
	if !reflect.DeepEqual(kinds, []string{"request", "response", "body", "error", "body", "body"}) || !bytes.Equal(logged, raw) {
		t.Fatalf("log order or body changed: %v; %q", kinds, logged)
	}
	if logs[0].Headers.Get("X-Source") != "actual" {
		t.Fatal("log headers not canonicalized")
	}
}

func TestMasterPreservesLargeRequestsLogsAndBody(t *testing.T) {
	input := strings.Repeat("x", (32<<20)+1)
	body, errMarshal := json.Marshal(map[string]any{"model": "test", "input": input})
	if errMarshal != nil {
		t.Fatal(errMarshal)
	}
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, m message) {
		var req Request
		if errDecode := json.Unmarshal(m.Params, &req); errDecode != nil || !bytes.Equal(req.Request, body) {
			t.Error("large request changed", errDecode)
			return
		}
		notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: req.RequestID, Kind: "request", Body: string(body)})
		accept(conn, m)
		sendBody(conn, []byte(input))
		notify(conn, "cpa/inference/completed", map[string]string{"requestId": req.RequestID})
	})
	client := dialMaster(t, endpoint)
	sawRequest := false
	client.OnUpstream = func(entry UpstreamLog) {
		if entry.Kind == "request" {
			sawRequest = true
			if entry.Body != string(body) {
				t.Error("large upstream request log changed")
			}
		}
	}
	req := request()
	req.Request = body
	if errStart := client.Start(req); errStart != nil {
		t.Fatal("large request or upstream log rejected", errStart)
	}
	if !sawRequest {
		t.Fatal("large upstream request log missing")
	}
	frame, errNext := client.Next()
	if errNext != nil || string(frame.Body) != input {
		t.Fatal("large response body changed or rejected", errNext)
	}
	if _, errNext = client.Next(); errNext != io.EOF {
		t.Fatal("large request completion missing", errNext)
	}
}

func TestBodyIsNotParsedAsResponsesEvents(t *testing.T) {
	payload := []byte("data: {\"type\":\"response.completed\"}\n\ndata: {\"type\":\"function_call\",\"arguments\":\"invalid {\"}\n\n")
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, m message) {
		accept(conn, m)
		sendBody(conn, payload)
		sendBody(conn, []byte{0xff, 0x00})
		notify(conn, "cpa/inference/completed", map[string]string{"requestId": "req"})
	})
	client := dialMaster(t, endpoint)
	if err := client.Start(request()); err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]byte{payload, {0xff, 0x00}} {
		frame, err := client.Next()
		if err != nil || !bytes.Equal(frame.Body, want) {
			t.Fatalf("body interpreted by bridge: %q, %v", frame.Body, err)
		}
	}
	if _, err := client.Next(); err != io.EOF {
		t.Fatal(err)
	}
}

func TestUpstreamLogsDoNotCountAsUnrelatedNotifications(t *testing.T) {
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, m message) {
		for range 140 {
			notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "request"})
			notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "response", StatusCode: 401})
		}
		accept(conn, m)
		for range 140 {
			notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "request"})
			notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "response", StatusCode: 200})
		}
		notify(conn, "cpa/inference/completed", map[string]string{"requestId": "req"})
	})
	client := dialMaster(t, endpoint)
	count := 0
	client.OnUpstream = func(UpstreamLog) { count++ }
	if err := client.Start(request()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Next(); err != io.EOF {
		t.Fatal(err)
	}
	if count != 560 {
		t.Fatalf("got %d logs, want 560", count)
	}
}

func TestMasterRejectsOldProtocolAndParsedEventCapabilities(t *testing.T) {
	for _, change := range []func(*Capabilities){
		func(c *Capabilities) { c.ProtocolVersion = 1 },
		func(c *Capabilities) { c.ProtocolVersion = 2 },
		func(c *Capabilities) { c.ExecutionMode = "agent" },
		func(c *Capabilities) { c.RawBody = false },
	} {
		caps := validCaps()
		change(&caps)
		endpoint := serveMaster(t, caps, func(*websocket.Conn, message) { t.Error("inference must not start") })
		client, err := Dial(context.Background(), endpoint)
		if client != nil {
			client.Close()
		}
		if err == nil {
			t.Fatal("accepted incompatible capabilities")
		}
	}
}

func TestBrokenBodyStreamIsARequestScopedError(t *testing.T) {
	for _, mode := range []string{"disconnect", "wrong-request", "old-event"} {
		t.Run(mode, func(t *testing.T) {
			endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, m message) {
				accept(conn, m)
				switch mode {
				case "wrong-request":
					notify(conn, "cpa/inference/body", map[string]any{"requestId": "other", "bodyBase64": []byte("data")})
				case "old-event":
					notify(conn, "cpa/inference/event", map[string]any{"requestId": "req", "event": map[string]string{"type": "response.created"}})
				}
			})
			client := dialMaster(t, endpoint)
			if err := client.Start(request()); err != nil {
				t.Fatal(err)
			}
			_, err := client.Next()
			var scoped *Error
			if !errors.As(err, &scoped) || !scoped.IsRequestScoped() || !scoped.IsCodexReplayUnsafe() {
				t.Fatalf("got %v, want non-replayable request error", err)
			}
		})
	}
}

func TestMasterHTTPErrorPreservesBodyAndHeaders(t *testing.T) {
	body := `{"error":{"message":"quota exceeded","code":"usage_limit_reached"}}`
	for _, beforeStart := range []bool{true, false} {
		for _, viaLog := range []bool{true, false} {
			name := "stream"
			if beforeStart {
				name = "start"
			}
			if viaLog {
				name += "/from-upstream-log"
			}
			t.Run(name, func(t *testing.T) {
				headers := http.Header{"retry-after": {"13"}, "x-request-id": {"http-error"}, "set-cookie": {"private"}}
				endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, m message) {
					if !beforeStart {
						accept(conn, m)
						sendBody(conn, []byte(": started\n\n"))
					}
					status := 429
					errBody, errHeaders := body, headers
					if viaLog {
						notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "response", StatusCode: 429, Body: body, Headers: headers})
						status, errBody, errHeaders = 502, "", nil
					}
					params := map[string]any{"requestId": "req", "httpStatus": status, "body": errBody, "headers": errHeaders, "message": "upstream failed"}
					if beforeStart {
						_ = conn.WriteJSON(map[string]any{"id": m.ID, "error": map[string]any{"code": -32000, "message": "upstream failed", "data": params}})
					} else {
						notify(conn, "cpa/inference/error", params)
					}
				})
				client := dialMaster(t, endpoint)
				err := client.Start(request())
				if !beforeStart {
					if err != nil {
						t.Fatal(err)
					}
					if _, err = client.Next(); err != nil {
						t.Fatal(err)
					}
					_, err = client.Next()
				}
				var upstream *Error
				if !errors.As(err, &upstream) || upstream.StatusCode() != 429 || string(upstream.ResponseBody()) != body {
					t.Fatalf("HTTP error changed: %#v", err)
				}
				if upstream.Headers().Get("Retry-After") != "13" || upstream.Headers().Get("X-Request-ID") != "http-error" || upstream.Headers().Get("Set-Cookie") != "" {
					t.Fatal("HTTP error headers changed", upstream.Headers())
				}
			})
		}
	}
}

func TestTransportStreamErrorIsNotNormalEOF(t *testing.T) {
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, m message) {
		accept(conn, m)
		sendBody(conn, []byte("data: partial"))
		notify(conn, "cpa/inference/upstream", UpstreamLog{RequestID: "req", Kind: "error", Message: "upstream stream reset"})
		notify(conn, "cpa/inference/error", map[string]any{"requestId": "req", "httpStatus": 502, "message": "upstream stream reset"})
	})
	client := dialMaster(t, endpoint)
	if err := client.Start(request()); err != nil {
		t.Fatal(err)
	}
	if _, err := client.Next(); err != nil {
		t.Fatal(err)
	}
	_, err := client.Next()
	if err == nil || errors.Is(err, io.EOF) || err.Error() != "upstream stream reset" {
		t.Fatalf("transport failure became completion: %v", err)
	}
}

func TestCancellationClosesOnlyRequestConnection(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{})
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, m message) {
		accept(conn, m)
		close(started)
		_, _, _ = conn.ReadMessage()
		close(closed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := Dial(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	closeCount := 0
	client.OnClose(func() { closeCount++ })
	if err = client.Start(request()); err != nil {
		t.Fatal(err)
	}
	<-started
	cancel()
	<-closed
	if _, err = client.Next(); !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
	client.Close()
	client.Close()
	if closeCount != 1 {
		t.Fatalf("OnClose invoked %d times", closeCount)
	}
	other := dialMaster(t, endpoint)
	if other.Caps.ProtocolVersion != ProtocolVersion {
		t.Fatal("another master connection was affected")
	}
}

func TestCancellationInterruptsStart(t *testing.T) {
	started, closed := make(chan struct{}), make(chan struct{})
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, _ message) {
		close(started)
		_, _, _ = conn.ReadMessage()
		close(closed)
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	client, err := Dial(ctx, endpoint)
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	result := make(chan error, 1)
	go func() { result <- client.Start(request()) }()
	<-started
	cancel()
	<-closed
	if err = <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("Start cancellation = %v", err)
	}
}

func TestExplicitReloadUsesOriginalCredentialID(t *testing.T) {
	endpoint := serveMaster(t, validCaps(), func(conn *websocket.Conn, m message) {
		if m.Method != "cpa/credential/reload" || string(m.Params) != `{"credentialId":"Group/Account-A.json"}` {
			t.Errorf("unexpected management request %s %s", m.Method, m.Params)
		}
		_ = conn.WriteJSON(map[string]any{"id": m.ID, "result": validCaps()})
	})
	client := dialMaster(t, endpoint)
	var caps Capabilities
	if err := client.Call("cpa/credential/reload", map[string]string{"credentialId": "Group/Account-A.json"}, &caps); err != nil {
		t.Fatal(err)
	}
	if !caps.ManualOAuth || !caps.RawBody || caps.ProtocolVersion != 3 {
		t.Fatal("reload did not return master capabilities", caps)
	}
}

func TestUnsupportedContinuationNeverSubmitted(t *testing.T) {
	endpoint := serveMaster(t, validCaps(), func(*websocket.Conn, message) { t.Error("unsupported inference submitted") })
	client := dialMaster(t, endpoint)
	for _, body := range []string{`{"previous_response_id":"old"}`, `{"generate":false}`} {
		req := request()
		req.Request = json.RawMessage(body)
		if client.Start(req) == nil {
			t.Fatal("unsupported continuation accepted")
		}
	}
}
