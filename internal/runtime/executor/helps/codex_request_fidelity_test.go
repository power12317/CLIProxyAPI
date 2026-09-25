package helps

import (
	"net/http"
	"strings"
	"testing"

	"github.com/tidwall/gjson"
)

func TestCodexNativeIncludeRemainsCallerOwned(t *testing.T) {
	for _, original := range []string{
		`{"client_metadata":{"x-codex-turn-metadata":"{}"}}`,
		`{"client_metadata":{"x-codex-turn-metadata":"{}"},"include":[],"service_tier":"default"}`,
		`{"client_metadata":{"ws_request_header_x_openai_internal_codex_responses_lite":false},"include":["web_search_call.action.sources"],"service_tier":"priority"}`,
	} {
		translated := []byte(`{"include":["reasoning.encrypted_content"]}`)
		got := PreserveCodexProtocolFields([]byte(original), translated)
		if gjson.GetBytes(got, "include").Raw != gjson.Get(original, "include").Raw {
			t.Errorf("include changed: %s", got)
		}
	}
}

func TestCodexProtocolFieldsCannotRestoreOrdinaryServiceTier(t *testing.T) {
	for _, tier := range []string{`null`, `""`, `"default"`, `"auto"`, `"standard"`, `"flex"`, `"unknown"`, `42`, `true`} {
		for _, native := range []bool{false, true} {
			original := []byte(`{"service_tier":` + tier + `,"include":[]}`)
			translated := []byte(`{"service_tier":` + tier + `}`)
			got := PreserveCodexProtocolFields(original, translated, native)
			if gjson.GetBytes(got, "service_tier").Exists() {
				t.Errorf("ordinary tier survived translation (native=%v): %s", native, got)
			}
		}
	}
	for _, tier := range []string{"default", "auto", "priority", "ultrafast"} {
		original := []byte(`{"service_tier":"` + tier + `"}`)
		got := PreserveCodexProtocolFields(original, []byte(`{}`), true)
		if gjson.GetBytes(got, "service_tier").Exists() {
			t.Errorf("service_tier must never be restored from the original body: %s", got)
		}
	}
	for _, tier := range []string{"priority", "ultrafast"} {
		translated := []byte(`{"service_tier":"` + tier + `"}`)
		got := PreserveCodexProtocolFields([]byte(`{"service_tier":"default"}`), translated, true)
		if gjson.GetBytes(got, "service_tier").String() != tier {
			t.Errorf("translated accelerated tier was overwritten by the original body: %s", got)
		}
	}
}

func TestCodexMetadataHeadersPreserveClientAndBodyIntent(t *testing.T) {
	body := []byte(`{"client_metadata":{"x-codex-parent-thread-id":"parent","x-openai-subagent":"collab_spawn","x-codex-turn-metadata":"{\"history_ingest_requested\":false,\"node_repl_auto_review_required\":false,\"tool_namespaces_info\":{\"large\":true},\"unknown\":7}"}}`)
	headers := http.Header{"X-Openai-Subagent": {"explicit-subagent"}}
	RestoreCodexMetadataHeaders(headers, body)
	if headers.Get("X-OpenAI-Subagent") != "explicit-subagent" || headers.Get("X-Codex-Parent-Thread-Id") != "parent" {
		t.Fatal(headers)
	}
	metadata := gjson.Parse(headers.Get("X-Codex-Turn-Metadata"))
	if metadata.Get("tool_namespaces_info").Exists() || metadata.Get("history_ingest_requested").Type != gjson.False || metadata.Get("unknown").Int() != 7 {
		t.Fatal(metadata.Raw)
	}
	if !strings.Contains(string(body), "tool_namespaces_info") {
		t.Fatal("body inventory changed")
	}
	headers.Set("X-Codex-Turn-Metadata", `{"history_ingest_requested":true,"custom":42,"installation_id":"old"}`)
	ApplyCodexOAuthHeaders(headers, CodexOAuthIdentity{InstallationID: "converged", TurnMetadataJSON: `{"history_ingest_requested":false}`}, true, "", "")
	metadata = gjson.Parse(headers.Get("X-Codex-Turn-Metadata"))
	if metadata.Get("history_ingest_requested").Type != gjson.True || metadata.Get("custom").Int() != 42 || metadata.Get("installation_id").String() != "converged" {
		t.Fatal(metadata.Raw)
	}
	if !strings.Contains(headers.Get("X-Codex-Beta-Features"), "remote_compaction_v2") {
		t.Fatal("existing beta behavior changed")
	}
}
