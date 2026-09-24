package util

import (
	"net/http"
	"testing"
)

func TestCodexLiteIntentDoesNotDependOnCatalog(t *testing.T) {
	for _, tc := range []struct{ name, body, header, want string }{
		{"native HTTP unknown model", `{"model":"future-model","client_metadata":{"x-codex-turn-metadata":"{}"},"reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`, "", "true"},
		{"shape without native marker", `{"model":"gpt-6-astra","reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`, "", ""},
		{"body false beats native shape", `{"client_metadata":{"x-codex-turn-metadata":"{}","ws_request_header_x_openai_internal_codex_responses_lite":false},"reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`, "", "false"},
		{"string false beats native shape", `{"client_metadata":{"x-codex-turn-metadata":"{}","ws_request_header_x_openai_internal_codex_responses_lite":"false"},"reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`, "", "false"},
		{"header false beats body true", `{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":"true"}}`, "false", "false"},
		{"header true beats body false", `{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":false}}`, "true", "true"},
		{"native missing intent", `{"client_metadata":{"x-codex-turn-metadata":"{}"}}`, "", ""},
		{"invalid mirror cannot trigger fallback", `{"client_metadata":{"x-codex-turn-metadata":"{}","ws_request_header_x_openai_internal_codex_responses_lite":123},"reasoning":{"context":"all_turns"},"parallel_tool_calls":false}`, "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			headers := make(http.Header)
			if tc.header != "" {
				headers.Set("X-OpenAI-Internal-Codex-Responses-Lite", tc.header)
			}
			if got := CodexResponsesLiteHeaderValue([]byte(tc.body), headers); got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
