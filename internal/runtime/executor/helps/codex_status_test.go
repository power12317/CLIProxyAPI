package helps

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/codexstatus"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexStatusObservesActualHTTPWebsocketAndRuntimeTraffic(t *testing.T) {
	for _, transport := range []string{"http", "websocket", "runtime"} {
		t.Run(transport, func(t *testing.T) {
			m := codexstatus.New(config.EventNotifications{})
			defer m.Close()
			r := m.Request()
			ctx := codexstatus.WithRequest(context.Background(), r)
			auth := &coreauth.Auth{ID: t.Name(), Provider: "codex"}
			defer InvalidateCodexTurnStates(auth.ID)
			headers := http.Header{"X-Codex-Turn-Metadata": {`{"turn_id":"actual-turn"}`}, "X-Codex-Turn-State": {strings.Repeat("r", 780)}}
			response := http.Header{"X-Codex-Turn-State": {strings.Repeat("s", 780)}}
			if transport == "runtime" {
				logger := NewCodexRuntimeLog(ctx, &config.Config{}, auth, "gpt-6.1-sol")
				logger.Record(bridge.UpstreamLog{Kind: "request", URL: "https://chatgpt.com/backend-api/codex/responses", Headers: headers})
				logger.Record(bridge.UpstreamLog{Kind: "response", Headers: response})
			} else {
				state := NewCodexTurnState(ctx, auth, "https://chatgpt.com/backend-api/codex/responses", nil, headers, "gpt-6.1-sol", nil)
				if transport == "websocket" {
					headers.Set(codexTurnStateHeader, "stale")
					state.ObserveRequest(headers, []byte(fmt.Sprintf(`{"client_metadata":{"turn_id":"final-turn","x-codex-turn-state":%q}}`, strings.Repeat("r", 780))))
					state.ObserveEvent([]byte(fmt.Sprintf(`{"type":"response.metadata","headers":{"x-codex-turn-state":%q}}`, strings.Repeat("s", 780))))
				} else {
					state.ObserveRequest(headers, nil)
					state.ObserveResponse(&http.Response{Header: response})
				}
			}
			if r.Status() != "degraded" {
				t.Fatal("actual upstream roundtrip did not mark degradation")
			}
		})
	}
}
