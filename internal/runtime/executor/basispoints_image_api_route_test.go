package executor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gorilla/websocket"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	core "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

func TestBasispointsImageAPIUsesNativeTransport(t *testing.T) {
	for _, model := range []string{"gpt-image-2.5", "custom-paint-alias"} {
		for _, operation := range []string{"generate", "edit-json", "edit-multipart"} {
			for _, stream := range []bool{false, true} {
				for _, forced := range []bool{false, true} {
					t.Run(fmt.Sprintf("%s/%s/stream=%t/forced=%t", model, operation, stream, forced), func(t *testing.T) {
						testBasispointsImageAPIRoute(t, model, operation, stream, forced)
					})
				}
			}
		}
	}
}

func testBasispointsImageAPIRoute(t *testing.T, model, operation string, stream, forced bool) {
	t.Helper()
	var websocketCalls atomic.Int32
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		websocketCalls.Add(1)
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer proxy.Close()
	cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}, ForceWebsocket: forced}}
	exec := NewCodexAutoExecutor(cfg)
	exec.basispointsExec.cooldown = basispoints.NewCooldown(nil)
	exec.basispointsExec.websocketDial = func(context.Context, string, http.Header, []string) (*websocket.Conn, *http.Response, error) {
		websocketCalls.Add(1)
		return nil, nil, fmt.Errorf("image request must not dial Basispoints")
	}
	wsConfig := *cfg
	wsConfig.ProxyURL = proxy.URL
	exec.wsExec = NewCodexWebsocketsExecutor(&wsConfig)
	path, upstreamPath := codexImagesGenerationsPath, "/backend-api/codex/images/generations"
	if operation != "generate" {
		path, upstreamPath = codexImagesEditsPath, "/backend-api/codex/images/edits"
	}
	if model == "custom-paint-alias" {
		upstreamPath = "/backend-api/codex/responses"
	}
	opts := codexOpenAIImageTestOptions(path, stream)
	source := map[string]any{"model": model, "prompt": "draw a flower", "stream": stream}
	if operation == "edit-json" {
		source["image"] = "data:image/png;base64,aW1hZ2U="
	}
	payload, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	if operation == "edit-multipart" {
		var body bytes.Buffer
		writer := multipart.NewWriter(&body)
		for key, value := range map[string]string{"model": model, "prompt": "draw a flower"} {
			if errWrite := writer.WriteField(key, value); errWrite != nil {
				t.Fatal(errWrite)
			}
		}
		part, errPart := writer.CreateFormFile("image[]", "source.png")
		if errPart != nil {
			t.Fatal(errPart)
		}
		if _, errWrite := part.Write([]byte("image")); errWrite != nil {
			t.Fatal(errWrite)
		}
		if errClose := writer.Close(); errClose != nil {
			t.Fatal(errClose)
		}
		payload = body.Bytes()
		opts.Headers = http.Header{"Content-Type": {writer.FormDataContentType()}}
	}
	before := bytes.Clone(payload)
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != http.MethodPost || r.URL.Host != "chatgpt.com" || r.URL.Path != upstreamPath {
			return nil, fmt.Errorf("unexpected image upstream: %s %s", r.Method, r.URL)
		}
		raw, errRead := io.ReadAll(r.Body)
		if errRead != nil {
			return nil, errRead
		}
		body := "{\"created\":1713833628,\"data\":[{\"b64_json\":\"AA==\"}]}"
		contentType := "application/json"
		if model == "custom-paint-alias" {
			if gjson.GetBytes(raw, "tool_choice.type").String() != "image_generation" || gjson.GetBytes(raw, "tools.0.model").String() != model {
				t.Error("native image compatibility request lost tool choice or image model")
			}
			body = "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"image-result\",\"status\":\"completed\",\"created_at\":1713833628,\"output\":[{\"type\":\"image_generation_call\",\"result\":\"AA==\",\"output_format\":\"png\"}]}}\n\n"
			contentType = "text/event-stream"
		} else if stream {
			body = "event: image_generation.completed\ndata: {\"type\":\"image_generation.completed\",\"b64_json\":\"AA==\"}\n\n"
			contentType = "text/event-stream"
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(body)), Request: r}, nil
	})))
	reportedWS := true
	ctx = core.WithUpstreamTransportCallback(ctx, func(ws bool) { reportedWS = ws })
	auth := newCodexOpenAIImageTestAuth("")
	req := core.Request{Model: model, Payload: payload}
	var output bytes.Buffer
	if stream {
		result, errStream := exec.ExecuteStream(ctx, auth, req, opts)
		if errStream != nil {
			t.Fatal(errStream)
		}
		for chunk := range result.Chunks {
			if chunk.Err != nil {
				t.Fatal(chunk.Err)
			}
			output.Write(chunk.Payload)
		}
	} else {
		result, errExecute := exec.Execute(ctx, auth, req, opts)
		if errExecute != nil {
			t.Fatal(errExecute)
		}
		output.Write(result.Payload)
	}
	if calls != 1 || websocketCalls.Load() != 0 || reportedWS {
		t.Fatalf("HTTP calls=%d WebSocket calls=%d reportedWS=%t", calls, websocketCalls.Load(), reportedWS)
	}
	if !strings.Contains(output.String(), "AA==") || !strings.Contains(output.String(), "b64_json") {
		t.Fatalf("image result missing: %s", output.String())
	}
	if !bytes.Equal(payload, before) {
		t.Fatal("caller image request was mutated")
	}
}
