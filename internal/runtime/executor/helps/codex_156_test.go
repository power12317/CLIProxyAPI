package helps

import (
	"bytes"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func mustNormalizeCodexLiteTools(t *testing.T, body []byte, images bool) []byte {
	t.Helper()
	got, err := NormalizeCodexLiteCompatibilityTools(body, images)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

func TestCodex156SeparatesSessionCacheThread(t *testing.T) {
	for _, child := range []bool{false, true} {
		body := []byte(`{"prompt_cache_key":"cache","client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session\",\"thread_id\":\"thread\",\"turn_id\":\"turn\",\"analytics_enabled\":false,\"future\":{\"value\":7}}"}}`)
		if child {
			raw := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()
			raw, _ = sjson.Set(raw, "subagent_kind", "thread_spawn")
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", raw)
		}
		updated, id, ok := ApplyCodexOAuthFidelity(body, "account", "windows", false)
		if !ok {
			t.Fatal("fidelity failed")
		}
		header := http.Header{"X-Codex-Beta-Features": {"other,remote_compaction_v2,other"}}
		ApplyCodexOAuthHeaders(header, id, "model", true, "", "")
		wantSession := "cache"
		if child {
			wantSession = "session"
		}
		if header.Get("Session-Id") != wantSession || header.Get("Thread-Id") != "thread" || header.Get("X-Client-Request-Id") != "thread" || header.Get("X-Codex-Window-Id") != "thread:0" {
			t.Fatalf("identifiers: %v", header)
		}
		metadata := gjson.Parse(gjson.GetBytes(updated, "client_metadata.x-codex-turn-metadata").String())
		if metadata.Get("session_id").String() != "session" || gjson.GetBytes(updated, "prompt_cache_key").String() != "cache" || metadata.Get("analytics_enabled").Type != gjson.False || metadata.Get("future.value").Int() != 7 {
			t.Fatalf("body: %s", updated)
		}
		if header.Get("Version") != "0.156.0" || !strings.Contains(header.Get("User-Agent"), "Windows") || header.Get("X-Codex-Beta-Features") != "other,remote_compaction_v2" {
			t.Fatalf("profile: %v", header)
		}
	}
	body, _, _ := ApplyCodexOAuthFidelity([]byte(`{"client_metadata":{"x-codex-turn-metadata":"{}"}}`), "account", "mac", true)
	if gjson.Parse(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()).Get("analytics_enabled").Exists() {
		t.Fatal("invented analytics state")
	}
}

func TestCodex156PreservesClientWindowAndHeaderProfile(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"cache","client_metadata":{"x-codex-turn-metadata":"{\"session_id\":\"session\",\"thread_id\":\"thread\",\"window_number\":7}"}}`)
	_, id, ok := ApplyCodexOAuthFidelity(body, "account", "mac", false)
	if !ok || id.WindowID != "thread:7" {
		t.Fatalf("window identity = %+v", id)
	}
	header := http.Header{"User-Agent": {"codex-tui/0.156.0 (Linux; x86_64)"}, "Originator": {"codex_exec"}, "Version": {"0.156.0-client"}}
	ApplyCodexOAuthHeaders(header, id, "model", true, "", "")
	if header.Get("Originator") != "codex_exec" || !strings.Contains(header.Get("User-Agent"), "Linux") || header.Get("Version") != "0.156.0-client" {
		t.Fatalf("client profile overwritten: %v", header)
	}
	ApplyCodexOAuthHeaders(header, id, "model", true, "configured-ua", "configured-beta")
	if header.Get("User-Agent") != "configured-ua" || header.Get("X-Codex-Beta-Features") != "configured-beta" {
		t.Fatalf("explicit configuration lost: %v", header)
	}
}

func TestCodex156CookieHTTPWSSScopeAndOwner(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "token", "account_id": "a", "email": "a@example.com"}}
	defer InvalidateCodexCookieJar(auth.ID)
	target, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	jar := CodexCookieJarForAuth(auth)
	jar.SetCookies(target, []*http.Cookie{{Name: "__oailb", Value: "http", Path: "/", Secure: true}, {Name: "session", Value: "not-allowed", Path: "/"}})
	wsURL := "wss://chatgpt.com/backend-api/codex/responses"
	if got := CodexWebsocketCookieHeaders(jar, wsURL, nil).Get("Cookie"); got != "__oailb=http" {
		t.Fatalf("HTTP -> WSS cookies = %q", got)
	}
	explicit := http.Header{"Cookie": {"__oailb=explicit"}}
	if got := CodexWebsocketCookieHeaders(jar, wsURL, explicit).Get("Cookie"); got != "__oailb=explicit" {
		t.Fatal(got)
	}
	for _, status := range []int{101, 403} {
		response := &http.Response{StatusCode: status, Header: http.Header{"Set-Cookie": {"__cf_bm=handshake; Path=/; Secure; Expires=Wed, 21 Oct 2037 07:28:00 GMT", "session=ignored; Path=/"}}}
		StoreCodexWebsocketCookies(jar, wsURL, response)
		if cookies := jar.Cookies(target); len(cookies) != 2 {
			t.Fatalf("WSS -> HTTP status=%d cookies=%v", status, cookies)
		}
		if StripCodexInternalResponseHeaders(response.Header).Get("Set-Cookie") != "" {
			t.Fatal("cookie leaked downstream")
		}
	}
	for _, wrong := range []string{"ws://chatgpt.com/backend-api/codex/responses", "wss://other.example/backend-api/codex/responses"} {
		if got := CodexWebsocketCookieHeaders(jar, wrong, nil).Get("Cookie"); got != "" {
			t.Fatal("wrong scope", got)
		}
	}
	other := auth.Clone()
	other.ID += "-other"
	defer InvalidateCodexCookieJar(other.ID)
	if len(CodexCookieJarForAuth(other).Cookies(target)) != 0 {
		t.Fatal("cross credential cookies")
	}
	auth.Metadata["access_token"] = "renewed"
	if CodexCookieJarForAuth(auth) != jar {
		t.Fatal("token refresh lost infrastructure cookies")
	}
	auth.Metadata["codex_cookie_preserve_oailb"] = false
	disabledJar := CodexCookieJarForAuth(auth)
	disabledJar.SetCookies(target, []*http.Cookie{{Name: "__oailb", Value: "reject"}})
	if len(disabledJar.Cookies(target)) != 0 {
		t.Fatal("explicit false ignored")
	}
	disabledJar.SetCookies(target, []*http.Cookie{{Name: "__cf_bm", Value: "old-owner"}})
	auth.Metadata["email"] = "new-owner@example.com"
	if len(CodexCookieJarForAuth(auth).Cookies(target)) != 0 {
		t.Fatal("owner inherited cookies")
	}
}

func TestCodex156WebsocketCookiePathAndExpiration(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "token"}}
	defer InvalidateCodexCookieJar(auth.ID)
	jar := CodexCookieJarForAuth(auth)
	target, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	jar.SetCookies(target, []*http.Cookie{
		{Name: "__oailb", Value: "valid", Path: "/backend-api/codex", Secure: true},
		{Name: "__cf_bm", Value: "expired", Path: "/", Expires: time.Unix(1, 0)},
		{Name: "__cflb", Value: "wrong-domain", Domain: "example.com", Path: "/"},
	})
	if got := CodexWebsocketCookieHeaders(jar, "wss://chatgpt.com/backend-api/codex/responses", nil).Get("Cookie"); got != "__oailb=valid" {
		t.Fatalf("cookies = %q", got)
	}
	if got := CodexWebsocketCookieHeaders(jar, "wss://chatgpt.com/unrelated", nil).Get("Cookie"); got != "" {
		t.Fatalf("path mismatch sent cookie = %q", got)
	}
}

func TestCodex156ConnectionFingerprintIgnoresTurnButTracksCredentials(t *testing.T) {
	auth := &cliproxyauth.Auth{Provider: "codex", Metadata: map[string]any{"access_token": "old", "account_id": "a"}}
	headers := http.Header{"Authorization": {"Bearer old"}, "User-Agent": {"codex"}}
	first := CodexConnectionFingerprint(auth, headers, "")
	for _, key := range []string{"Cookie", "Session-Id", "X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Client-Request-Id"} {
		headers.Set(key, "new-turn")
	}
	if first != CodexConnectionFingerprint(auth, headers, "") {
		t.Fatal("per-turn update reconnects")
	}
	headers.Set("Authorization", "Bearer renewed")
	if first == CodexConnectionFingerprint(auth, headers, "") {
		t.Fatal("renewed credential reuses old handshake")
	}
	headers.Set("Authorization", "Bearer old")
	auth.Metadata["account_id"] = "b"
	if first == CodexConnectionFingerprint(auth, headers, "") {
		t.Fatal("owner change reuses connection")
	}
}

func TestCodex156LiteToolsIdempotentNamespaceMerge(t *testing.T) {
	body := []byte(`{"prompt_cache_key":"s","instructions":"Guide","tools":[{"type":"web_search_preview"},{"type":"image_generation"}],"input":[{"type":"additional_tools","id":"at_old","role":"developer","tools":[{"type":"namespace","name":"web","description":"keep","tools":[{"type":"function","name":"open","parameters":{}}]}]},{"type":"function_call_output","call_id":"c","output":[{"type":"input_image","image_url":"data:image/png;base64,AA=="}]}]}`)
	got := mustNormalizeCodexLiteTools(t, body, true)
	if util.ClassifyCodexResponsesLiteTools(got) != util.CodexResponsesLiteToolsCompatible || gjson.GetBytes(got, "tools").Exists() || gjson.GetBytes(got, "instructions").Exists() {
		t.Fatalf("invalid Lite: %s", got)
	}
	if !codexDeclaredFunction(got, "image_gen", "imagegen") || !codexDeclaredFunction(got, "web", "run") || !codexDeclaredFunction(got, "web", "open") {
		t.Fatal(string(got))
	}
	second := mustNormalizeCodexLiteTools(t, got, true)
	if !bytes.Equal(got, second) {
		t.Fatalf("duplicate/reidentified declarations: %s", second)
	}
	foundOld := false
	for _, item := range gjson.GetBytes(got, "input").Array() {
		if item.Get("id").String() == "at_old" {
			foundOld = true
		}
	}
	if foundOld {
		t.Fatal("changed tools retained obsolete item identity")
	}
	if !bytes.Contains(got, []byte(`"call_id":"c"`)) || !bytes.Contains(got, []byte(`data:image/png;base64,AA==`)) {
		t.Fatal("tool output lost")
	}
	stripped := StripCodexImageTools(got)
	if codexDeclaredFunction(stripped, "image_gen", "imagegen") || !codexDeclaredFunction(stripped, "web", "run") {
		t.Fatalf("strip: %s", stripped)
	}
}

func TestCodex156StripImageNamespacePreservesOtherToolsAndSchema(t *testing.T) {
	body := []byte(`{"tools":[{"type":"namespace","name":"image_gen","tools":[{"type":"function","name":"imagegen"},{"type":"function","name":"inspect","parameters":{"properties":{"tools":{"default":[{"type":"image_generation"}]}}}}]},{"type":"function","name":"image_gen__imagegen"}],"tool_choice":{"type":"function","namespace":"image_gen","name":"imagegen"}}`)
	got := StripCodexImageTools(body)
	if gjson.GetBytes(got, "tools.0.tools.#").Int() != 1 || gjson.GetBytes(got, "tools.0.tools.0.name").String() != "inspect" || gjson.GetBytes(got, "tools.0.tools.0.parameters.properties.tools.default.0.type").String() != "image_generation" || gjson.GetBytes(got, "tool_choice").Exists() {
		t.Fatalf("strip: %s", got)
	}
}

func TestCodex156TurnStateOwnerReplacementRejectsOldResponses(t *testing.T) {
	cache := newCodexTurnStateCache(nil)
	cache.now = time.Now
	auth := &cliproxyauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"email": "a@example.com", "account_id": "workspace"}}
	first := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("same-turn"), nil)
	first.observe("owner-a")
	auth.Metadata["email"] = "b@example.com"
	second := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("same-turn"), nil)
	if _, ok := second.cached(); ok {
		t.Fatal("owner b inherited owner a turn state")
	}
	first.observe("late-owner-a")
	second.observe("owner-b")
	auth.Metadata["email"] = "a@example.com"
	third := cache.request(auth, "https://chatgpt.com/backend-api/codex/responses", turnStateBody("same-turn"), nil)
	if _, ok := third.cached(); ok {
		t.Fatal("retired owner state resurrected")
	}
}

func TestCodex156LiteHistoryNamesAndClientSchemasPreserved(t *testing.T) {
	body := []byte(`{"input":[{"type":"additional_tools","id":"at-client","tools":[{"type":"namespace","name":"web","tools":[{"type":"function","name":"run","description":"full client schema","parameters":{"properties":{"open":{"type":"array"}}}}]}]},{"type":"function_call","name":"image_gen__imagegen","call_id":"image","arguments":"{\"prompt\":\"test\"}"},{"type":"function_call_output","call_id":"image","output":"result"}]}`)
	body, _ = sjson.SetRawBytes(body, "input.0.tools.0", []byte(codexLiteWebNamespace))
	got := mustNormalizeCodexLiteTools(t, body, true)
	found := false
	for _, item := range gjson.GetBytes(got, "input").Array() {
		if item.Get("id").String() == "at-client" {
			found = true
			if !codexToolJSONEqual(item.Get("tools.0").Raw, codexLiteWebNamespace) {
				t.Fatal("schema overwritten")
			}
		}
		if item.Get("type").String() == "function_call" {
			if item.Get("namespace").String() != "image_gen" || item.Get("name").String() != "imagegen" || item.Get("call_id").String() != "image" {
				t.Fatal(string(got))
			}
		}
	}
	if !found {
		t.Fatal("unchanged client declaration identity lost")
	}
}

func TestCodex156LiteFlatDeclarationsKeepFlatHistory(t *testing.T) {
	body := []byte(`{"instructions":"","tools":[{"type":"function","name":"image_gen__imagegen","description":"client schema","parameters":{}}],"tool_choice":{"type":"image_generation"},"input":[{"type":"function_call","name":"image_gen__imagegen","call_id":"c","arguments":"{}"}]}`)
	canonical, _ := sjson.Set(gjson.Parse(codexLiteImageNamespace).Get("tools.0").Raw, "name", "image_gen__imagegen")
	body, _ = sjson.SetRawBytes(body, "tools.0", []byte(canonical))
	got := mustNormalizeCodexLiteTools(t, body, true)
	if gjson.GetBytes(got, "input.0.tools.#").Int() != 1 || !codexToolJSONEqual(gjson.GetBytes(got, "input.0.tools.0").Raw, canonical) || gjson.GetBytes(got, "input.1.namespace").Exists() || gjson.GetBytes(got, "input.1.name").String() != "image_gen__imagegen" {
		t.Fatalf("flat declaration/history changed: %s", got)
	}
	if gjson.GetBytes(got, "tool_choice.name").String() != "image_gen__imagegen" || gjson.GetBytes(got, "tool_choice.namespace").Exists() {
		t.Fatalf("choice mismatches actual declaration: %s", got)
	}
}

func TestCodex156LiteNormalizationRetainsInvalidInput(t *testing.T) {
	for _, raw := range []string{
		`{"instructions":"guide","input":"original text","tools":[]}`,
		`{"instructions":"guide","input":{},"tools":[]}`,
		`{"input":[],"tools":{}}`,
	} {
		if got := mustNormalizeCodexLiteTools(t, []byte(raw), false); string(got) != raw {
			t.Fatalf("invalid input silently discarded: %s", got)
		}
	}
}

func TestCodex156LiteAllowedToolChoicesFollowDeclarations(t *testing.T) {
	body := []byte(`{"input":[],"tools":[{"type":"image_generation"},{"type":"web_search_preview"},{"type":"function","name":"exec"}],"tool_choice":{"type":"allowed_tools","mode":"auto","tools":[{"type":"image_generation"},{"type":"web_search_preview"},{"type":"function","name":"exec"}]}}`)
	got := mustNormalizeCodexLiteTools(t, body, true)
	choice := gjson.GetBytes(got, "tool_choice")
	if choice.Get("mode").String() != "auto" || choice.Get("tools.0.namespace").String() != "image_gen" || choice.Get("tools.1.namespace").String() != "web" || choice.Get("tools.2.name").String() != "exec" {
		t.Fatalf("choices do not match Lite declarations: %s", got)
	}
}
