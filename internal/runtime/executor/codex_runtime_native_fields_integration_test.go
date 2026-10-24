package executor

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	openaihandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers/openai"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func testCodexRuntimeLiteParallelCalls(t *testing.T, cfg *config.Config, auth *coreauth.Auth, readUpstream func(*testing.T) []byte) int {
	t.Helper()
	count := 0
	for _, source := range []sdktranslator.Format{sdktranslator.FormatOpenAIResponse, sdktranslator.FormatOpenAI} {
		for _, setting := range []string{"omitted", "false", "true"} {
			t.Run(fmt.Sprintf("%s/%s", source, setting), func(t *testing.T) {
				body := []byte(`{"input":"native-lite-parallel-probe"}`)
				if source == sdktranslator.FormatOpenAI {
					body = []byte(`{"messages":[{"role":"user","content":"native-lite-parallel-probe"}]}`)
				}
				if setting != "omitted" {
					body, _ = sjson.SetRawBytes(body, "parallel_tool_calls", []byte(setting))
				}
				opts := runtimeTestOptions()
				opts.SourceFormat = source
				// No Lite hint is supplied. The worker's native model owns the mode.
				_, errExecute := runtimeImageResult(t, NewCodexAutoExecutor(cfg), t.Context(), auth, coreexecutor.Request{Model: "gpt-6.1-sol", Payload: body}, opts, false)
				if errExecute != nil {
					t.Fatal(errExecute)
				}
				actual := readUpstream(t)
				if gjson.GetBytes(actual, "parallel_tool_calls").Type != gjson.False {
					t.Fatalf("native Lite normalization was bypassed for %s: %s", setting, actual)
				}
				t.Logf("caller %s parallel=%s without Lite hint -> native Lite parallel=false", source, setting)
			})
			count++
		}
	}
	return count
}

// The corpus is generated from native Rust values with exhaustive enum matches
// and field-complete constructors. It is not a second list of admitted types.
func testCodexRuntimeNativeFieldReplay(t *testing.T, cfg *config.Config, auth *coreauth.Auth, router *gin.Engine, readUpstream func(*testing.T) []byte) int {
	t.Helper()
	fixture := os.Getenv("CODEX_RUNTIME_HISTORY_FIXTURE")
	if fixture == "" {
		t.Skip("set CODEX_RUNTIME_HISTORY_FIXTURE to the native-generated history corpus")
	}
	raw, errRead := os.ReadFile(fixture)
	if errRead != nil {
		t.Fatal(errRead)
	}
	history := gjson.ParseBytes(raw)
	if !history.IsArray() {
		history = history.Get("history")
	}
	if !history.IsArray() || len(history.Array()) == 0 {
		t.Fatal("native history corpus must contain a nonempty history array")
	}
	expectedHistory := history
	if expectedFixture := os.Getenv("CODEX_RUNTIME_HISTORY_EXPECTED_FIXTURE"); expectedFixture != "" {
		expectedRaw, errExpected := os.ReadFile(expectedFixture)
		if errExpected != nil {
			t.Fatal(errExpected)
		}
		expectedHistory = gjson.ParseBytes(expectedRaw)
		if !expectedHistory.IsArray() {
			t.Fatal("native formatter expectation must be a history array")
		}
	}
	if router == nil {
		manager := coreauth.NewManager(nil, nil, nil)
		manager.SetConfig(cfg)
		manager.RegisterExecutor(NewCodexAutoExecutor(cfg))
		if _, errRegister := manager.Register(t.Context(), auth); errRegister != nil {
			t.Fatal(errRegister)
		}
		registry.GetGlobalRegistry().RegisterClient(auth.ID, "codex", []*registry.ModelInfo{{ID: "gpt-6.1-sol"}})
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(auth.ID) })
		handler := openaihandlers.NewOpenAIResponsesAPIHandler(handlers.NewBaseAPIHandlers(&cfg.SDKConfig, manager))
		router = gin.New()
		router.Use(func(c *gin.Context) {
			c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), "cliproxy.roundtripper", http.DefaultTransport))
			c.Next()
		})
		router.POST("/v1/responses", handler.Responses)
	}
	server := httptest.NewServer(router)
	defer server.Close()
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	client := &http.Client{Transport: transport}
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%t", stream), func(t *testing.T) {
			body, _ := sjson.SetRawBytes([]byte(`{"model":"gpt-6.1-sol"}`), "input", []byte(history.Raw))
			body, _ = sjson.SetBytes(body, "stream", stream)
			req, errRequest := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/v1/responses", strings.NewReader(string(body)))
			if errRequest != nil {
				t.Fatal(errRequest)
			}
			req.Header.Set("Content-Type", "application/json")
			response, errDo := client.Do(req)
			if errDo != nil {
				t.Fatal(errDo)
			}
			defer func() { _ = response.Body.Close() }()
			output, errBody := io.ReadAll(response.Body)
			if errBody != nil || response.StatusCode != http.StatusOK {
				t.Fatalf("native field replay failed: status=%d err=%v body=%s", response.StatusCode, errBody, output)
			}
			actual := readUpstream(t)
			items := gjson.GetBytes(actual, "input").Array()
			next := 0
			for index, expected := range expectedHistory.Array() {
				found := -1
				for candidate := next; candidate < len(items); candidate++ {
					item := items[candidate]
					if item.Get("type").String() != expected.Get("type").String() {
						continue
					}
					if id := expected.Get("id"); id.Exists() && id.Raw != item.Get("id").Raw {
						continue
					}
					found = candidate
					break
				}
				if found < 0 {
					t.Fatalf("native history item %d (%s, id=%s) was discarded or reordered: %s", index, expected.Get("type"), expected.Get("id"), actual)
				}
				assertCodexNativeFields(t, fmt.Sprintf("input[%d]", index), json.RawMessage(expected.Raw), json.RawMessage(items[found].Raw))
				next = found + 1
			}
			t.Logf("real CPA HTTP entry -> master -> worker -> upstream: %d source items, %d native-formatted history items checked", len(history.Array()), len(expectedHistory.Array()))
		})
	}
	return 2
}

// Extra native-generated fields are allowed. Known native corpus values must
// survive adaptation, including optional statuses and opaque nested business JSON.
func assertCodexNativeFields(t *testing.T, path string, expected, actual json.RawMessage) {
	t.Helper()
	var expectedObject, actualObject map[string]json.RawMessage
	if json.Unmarshal(expected, &expectedObject) == nil && expectedObject != nil {
		if json.Unmarshal(actual, &actualObject) != nil || actualObject == nil {
			t.Fatalf("native object %s was replaced: %s", path, actual)
		}
		for key, value := range expectedObject {
			got, exists := actualObject[key]
			if !exists {
				t.Fatalf("native field %s.%s was discarded", path, key)
			}
			assertCodexNativeFields(t, path+"."+key, value, got)
		}
		return
	}
	var expectedArray, actualArray []json.RawMessage
	if json.Unmarshal(expected, &expectedArray) == nil && expectedArray != nil {
		if json.Unmarshal(actual, &actualArray) != nil || len(expectedArray) != len(actualArray) {
			t.Fatalf("native array %s changed: expected=%s actual=%s", path, expected, actual)
		}
		for index, value := range expectedArray {
			assertCodexNativeFields(t, fmt.Sprintf("%s[%d]", path, index), value, actualArray[index])
		}
		return
	}
	equalRuntimeNativeJSON(t, expected, actual)
}
