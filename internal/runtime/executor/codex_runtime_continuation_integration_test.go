package executor

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	"github.com/tidwall/gjson"
)

// The client sends only deltas; the mock upstream observes native wire requests.
func testNativeRuntimeContinuation(t *testing.T, handler *openaihandlers.OpenAIResponsesAPIHandler, transport string, readExchange func(*testing.T) nativeRuntimeExchange) {
	t.Helper()
	router := gin.New()
	router.GET("/v1/responses", func(c *gin.Context) {
		c.Set("userApiKey", "synthetic-continuation-caller")
		handler.ResponsesWebsocket(c)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	conn, _, err := websocket.DefaultDialer.DialContext(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()
	previous, cacheKey, initialWindow := "", "", ""
	for round := range 7 {
		turn, window := "turn-1", "window-0"
		if round >= 2 {
			turn = "turn-2"
		}
		instructions := "continuation-fixture"
		if round >= 3 {
			instructions += " changed instructions"
		}
		input := []any{map[string]any{"type": "message", "role": "user", "content": fmt.Sprintf("native-continuation-step-%d", round)}}
		wantItems := round*2 + 1
		if round >= 5 {
			turn, window = "turn-3", "window-1"
			wantItems = 2 + (round-5)*2
		}
		if round == 5 {
			input = append([]any{map[string]any{"type": "compaction", "encrypted_content": "client-compacted-window"}}, input...)
		}
		request := map[string]any{
			"type": "response.create", "model": "gpt-6.1-sol", "instructions": instructions, "input": input,
			"client_metadata": map[string]any{"session_id": t.Name(), "thread_id": t.Name(), "turn_id": turn, "window_id": window},
		}
		if round > 0 && round != 5 {
			request["previous_response_id"] = previous
		}
		if round >= 4 {
			request["tools"] = []any{map[string]any{"type": "function", "name": "client_lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{}}}}
		}
		if err := conn.WriteJSON(request); err != nil {
			t.Fatal(err)
		}
		lastResponse := previous
		for {
			_, event, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("round %d: %v", round+1, err)
			}
			kind := gjson.GetBytes(event, "type").String()
			if kind == "error" || kind == "response.failed" {
				t.Fatalf("round %d: %s", round+1, event)
			}
			if kind == "response.completed" {
				previous = gjson.GetBytes(event, "response.id").String()
				break
			}
		}
		actual := readExchange(t)
		wantPrevious := ""
		if transport == "websocket" && (round == 1 || round == 2 || round == 6) {
			wantItems, wantPrevious = 1, lastResponse
		}
		var businessItems []gjson.Result
		for _, item := range gjson.GetBytes(actual.body, "input").Array() {
			// Responses Lite also carries native instructions and tool definitions
			// in input. Count the caller's history separately from those items.
			if item.Get("role").String() != "developer" {
				businessItems = append(businessItems, item)
			}
		}
		if actual.websocket != (transport == "websocket") || len(businessItems) != wantItems || gjson.GetBytes(actual.body, "previous_response_id").String() != wantPrevious {
			t.Fatalf("round %d: expected native input=%d parent=%q transport=%s, got %s", round+1, wantItems, wantPrevious, transport, actual.body)
		}
		if transport == "http" && actual.headers.Get("Content-Encoding") != "zstd" {
			t.Fatalf("native HTTP compression lost: %v", actual.headers)
		}
		key := gjson.GetBytes(actual.body, "prompt_cache_key").String()
		if key == "" || round > 0 && key != cacheKey {
			t.Fatalf("native prompt cache identity changed: %q -> %q", cacheKey, key)
		}
		cacheKey = key
		turnMetadata := gjson.Parse(gjson.GetBytes(actual.body, "client_metadata.x-codex-turn-metadata").String())
		actualWindow := turnMetadata.Get("window_id").String()
		if round == 0 {
			initialWindow = actualWindow
		}
		if actualWindow == "" || round < 5 && actualWindow != initialWindow || round >= 5 && actualWindow == initialWindow {
			t.Fatalf("round %d: native window did not follow compaction: initial=%q current=%q", round+1, initialWindow, actualWindow)
		}
		if round == 5 && businessItems[0].Get("encrypted_content").String() != "client-compacted-window" {
			t.Fatalf("compaction window lost: %s", actual.body)
		}
		t.Logf("round=%d turn=%s input=%d parent=%q websocket=%t encoding=%q window=%s", round+1, turn, wantItems, wantPrevious, actual.websocket, actual.headers.Get("Content-Encoding"), actualWindow)
	}
}
