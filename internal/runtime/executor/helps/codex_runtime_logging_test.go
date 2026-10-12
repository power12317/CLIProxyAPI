package helps

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
)

func TestCodexRuntimeNodeLogsPreserveRequestAndExchange(t *testing.T) {
	for _, storage := range []string{"memory", "file", "deferred"} {
		for _, tc := range []struct{ name, request, response string }{
			{"request fallback reported by worker", "unified-96", "unified-96"},
			{"response replaces request", "unified-96", "unified-42"},
			{"response only", "", "unified-42"},
			{"no node", "", ""},
			{"response explicitly clears node", "unified-96", ""},
		} {
			t.Run(storage+"/"+tc.name, func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				reporter := &UsageReporter{}
				ctx := WithCodexOaiLBReporter(context.WithValue(context.Background(), "gin", c), reporter)
				cfg := &config.Config{SDKConfig: config.SDKConfig{RequestLog: storage != "deferred"}}
				var source *logging.FileBodySource
				if storage == "file" {
					var errSource error
					source, errSource = logging.NewFileBodySourceInDir(t.TempDir(), "api-request")
					if errSource != nil {
						t.Fatal(errSource)
					}
					t.Cleanup(func() {
						if errCleanup := source.Cleanup(); errCleanup != nil {
							t.Error(errCleanup)
						}
					})
					c.Set(logging.APIRequestSourceContextKey, source)
				}
				logger := NewCodexRuntimeLog(ctx, cfg, &coreauth.Auth{ID: "account-a", FileName: "account-a.json"}, "test-model")
				logger.Record(bridge.UpstreamLog{
					Kind: "request", URL: "https://chatgpt.com/backend-api/codex/responses", Method: http.MethodPost,
					Headers: http.Header{"Cookie": {"[REDACTED]"}}, Body: `{"input":[]}`, OaiLBNode: tc.request,
				})
				if got := logging.CodexOaiLBNode(c); got != tc.request {
					t.Fatalf("request node = %q, want %q", got, tc.request)
				}
				logger.Record(bridge.UpstreamLog{Kind: "response", StatusCode: http.StatusOK, OaiLBNode: tc.response})
				if got := logging.CodexOaiLBNode(c); got != tc.response {
					t.Fatalf("access-log node = %q, want %q", got, tc.response)
				}
				if got := reporter.OaiLBNode(); got != tc.response {
					t.Fatalf("existing reporter node = %q, want %q", got, tc.response)
				}

				var request string
				switch storage {
				case "file":
					data, errBytes := source.Bytes()
					if errBytes != nil {
						t.Fatal(errBytes)
					}
					request = string(data)
				case "deferred":
					value, _ := c.Get(logging.DeferredAPIRequestContextKey)
					requests := value.([]logging.DeferredAPIRequest)
					request = string(requests[0]())
				default:
					value, _ := c.Get(apiRequestKey)
					request = string(value.([]byte))
				}
				if tc.request == "" {
					if strings.Contains(request, "oailb_node:") {
						t.Fatalf("request acquired a node from its response: %s", request)
					}
				} else {
					index := strings.Index(request, "oailb_node: "+tc.request+"\n")
					if index < 0 || index > strings.Index(request, "\nHeaders:\n") {
						t.Fatalf("request node is missing from request metadata: %s", request)
					}
				}
				if !strings.Contains(request, "\nBody:\n{\"input\":[]}\n\n") {
					t.Fatalf("node metadata changed the recorded request body: %s", request)
				}
				if storage != "deferred" {
					value, _ := c.Get(apiResponseKey)
					response := string(value.([]byte))
					if tc.response == "" {
						if strings.Contains(response, "oailb_node:") {
							t.Fatalf("response retained a cleared node: %s", response)
						}
					} else if !strings.Contains(response, "oailb_node: "+tc.response+"\n") {
						t.Fatalf("response node missing: %s", response)
					}
				}
			})
		}
	}
}

func TestCodexRuntimeNodeLogsSeparateAccountsAndAttempts(t *testing.T) {
	var contexts []*gin.Context
	var loggers []*CodexRuntimeLog
	for _, account := range []string{"account-a", "account-b"} {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		contexts = append(contexts, c)
		ctx := context.WithValue(context.Background(), "gin", c)
		loggers = append(loggers, NewCodexRuntimeLog(ctx, &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}, &coreauth.Auth{ID: account, FileName: account + ".json"}, "test-model"))
	}
	loggers[0].Record(bridge.UpstreamLog{Kind: "request", OaiLBNode: "unified-96"})
	loggers[1].Record(bridge.UpstreamLog{Kind: "request", OaiLBNode: "unified-42"})
	loggers[0].Record(bridge.UpstreamLog{Kind: "response", StatusCode: http.StatusUnauthorized, OaiLBNode: "unified-96"})
	loggers[0].Record(bridge.UpstreamLog{Kind: "request"})
	if got := logging.CodexOaiLBNode(contexts[0]); got != "" {
		t.Fatalf("new attempt inherited an earlier request node: %q", got)
	}
	loggers[0].Record(bridge.UpstreamLog{Kind: "response", StatusCode: http.StatusTooManyRequests})
	loggers[1].Record(bridge.UpstreamLog{Kind: "response", StatusCode: http.StatusOK, OaiLBNode: "unified-42"})
	if got := logging.CodexOaiLBNode(contexts[1]); got != "unified-42" {
		t.Fatalf("another account changed the node: %q", got)
	}
	value, _ := contexts[0].Get(apiRequestKey)
	requests := strings.Split(string(value.([]byte)), "=== API REQUEST 2 ===")
	if len(requests) != 2 || !strings.Contains(requests[0], "oailb_node: unified-96") || strings.Contains(requests[1], "oailb_node:") {
		t.Fatalf("request node metadata leaked between attempts: %s", value)
	}
}

func TestCodexRuntimeReportsNativeTransportAttempts(t *testing.T) {
	var transports []bool
	ctx := coreexecutor.WithUpstreamTransportCallback(context.Background(), func(websocket bool) {
		transports = append(transports, websocket)
	})
	logger := NewCodexRuntimeLog(ctx, &config.Config{}, &coreauth.Auth{ID: "account"}, "test-model")
	for _, transport := range []string{"websocket", "http", "", "websocket"} {
		logger.Record(bridge.UpstreamLog{Kind: "request", Transport: transport})
	}
	if !reflect.DeepEqual(transports, []bool{true, false, true}) {
		t.Fatalf("native transport attempts changed: %v", transports)
	}
}

func TestCodexRuntimeLogsNativeCompressedRequestReadably(t *testing.T) {
	body := []byte(`{"input":[{"role":"user","content":"native compressed business request"}],"client_metadata":{"turn_id":"native-turn"}}`)
	encoder, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	encoded := encoder.EncodeAll(body, nil)
	if errClose := encoder.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	original := bytes.Clone(encoded)
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx := context.WithValue(context.Background(), "gin", c)
	logger := NewCodexRuntimeLog(ctx, &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}, &coreauth.Auth{ID: "account"}, "test-model")
	logger.Record(bridge.UpstreamLog{
		Kind: "request", Transport: "http", Method: "POST", URL: "https://example.invalid/responses",
		Headers: http.Header{"Content-Encoding": {"zstd"}}, Body: "[encoded native request body]", BodyBytes: encoded,
	})
	value, _ := c.Get(apiRequestKey)
	if !strings.Contains(string(value.([]byte)), string(body)) || !bytes.Equal(encoded, original) {
		t.Fatal("native request log lost business data or mutated the observed bytes")
	}
}

func TestCodexRuntimeObservesActualRoutingMetadataBeforeNativeCompletion(t *testing.T) {
	for _, transport := range []string{"http", "websocket"} {
		t.Run(transport, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx := context.WithValue(context.Background(), "gin", c)
			logger := NewCodexRuntimeLog(ctx, &config.Config{SDKConfig: config.SDKConfig{RequestLog: true}}, &coreauth.Auth{ID: "account"}, "test-model")
			logger.RawBody = true
			body := `{"client_metadata":{"turn_id":"native-turn"}}`
			wantRequest := "request-ticket"
			if transport == "websocket" {
				body = `{"type":"response.create","client_metadata":{"turn_id":"native-turn","x-codex-turn-state":"frame-ticket"}}`
				wantRequest = "frame-ticket"
			}
			logger.Record(bridge.UpstreamLog{Kind: "request", Transport: transport, URL: "https://chatgpt.com/backend-api/codex/responses", Headers: http.Header{"X-Codex-Turn-State": {"request-ticket"}}, Body: body})
			logger.Record(bridge.UpstreamLog{Kind: "response", StatusCode: http.StatusOK})
			event := []byte(`{"type":"response.metadata","headers":{"x-codex-turn-state":"new-native-ticket"}}`)
			if transport == "websocket" {
				logger.Record(bridge.UpstreamLog{Kind: "body", Transport: transport, BodyBytes: event})
			} else {
				wire := []byte("event: response.metadata\r\ndata: {\r\ndata: \"type\":\"response.metadata\",\r\ndata: \"headers\":{\"x-codex-turn-state\":\"new-native-ticket\"}}\r\n\r\n")
				for _, b := range wire {
					logger.Record(bridge.UpstreamLog{Kind: "body", Transport: transport, BodyBytes: []byte{b}})
				}
			}
			// No final business output is needed to record an actual native attempt.
			fields, ok := logging.CodexTurnStateLogFieldsForContext(c)
			if !ok || fields.TurnID != "native-turn" || fields.RequestTurnStateLen != len(wantRequest) || fields.ResponseTurnStateLen != len("new-native-ticket") {
				t.Fatalf("native routing metadata was lost: %+v", fields)
			}
			logger.Event(event)
			logger.Flush()
			value, _ := c.Get(apiResponseKey)
			if strings.Count(string(value.([]byte)), "new-native-ticket") != 1 {
				t.Fatal("business delivery duplicated the native diagnostic body")
			}
		})
	}
}
