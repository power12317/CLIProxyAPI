package helps

import (
	"net/http"
	"net/url"
	"testing"

	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestApplyCodexOAuthFidelityReconstructsIdentityAndWindowsSystem(t *testing.T) {
	body := []byte(`{"model":"gpt-5.6-terra","prompt_cache_key":"session-1","tools":[{"type":"function","description":"Windows safety rules: do not delete files"}],"client_metadata":{"x-codex-turn-metadata":"{\"sandbox\":\"linux\",\"turn_id\":\"turn-1\"}"}}`)
	updated, identity, ok := ApplyCodexOAuthFidelity(body, "acct-1")
	if !ok {
		t.Fatal("expected fidelity rewrite")
	}
	if identity.System != "windows" {
		t.Fatalf("system = %q, want windows", identity.System)
	}
	if identity.SessionID != "session-1" || identity.TurnID != "turn-1" {
		t.Fatalf("identity = %+v", identity)
	}
	if got := gjson.GetBytes(updated, "client_metadata.x-codex-turn-metadata").String(); got == "" {
		t.Fatal("missing canonical turn metadata")
	}
	if got := gjson.GetBytes(updated, "client_metadata.x-codex-installation-id").String(); got == "" {
		t.Fatal("missing installation identity")
	}
}

func TestApplyCodexOAuthFidelityDefaultsMacAndTimezone(t *testing.T) {
	body := []byte(`{"client_metadata":{"x-codex-turn-metadata":"{\"sandbox\":\"other\",\"environment_context\":{\"cwd\":\"/tmp\",\"shell\":\"zsh\"}}"},"input":[{"type":"input_text","text":"environment_context <timezone>Asia/Shanghai</timezone>"}]}`)
	updated, identity, ok := ApplyCodexOAuthFidelity(body, "acct-2")
	if !ok || identity.System != "mac" {
		t.Fatalf("rewrite=%t identity=%+v", ok, identity)
	}
	if got := gjson.GetBytes(updated, "input.0.text").String(); got == "" || got == "environment_context <timezone>Asia/Shanghai</timezone>" {
		t.Fatalf("timezone was not normalized: %q", got)
	}
}

func TestCodexCookieJarIsScopedAndFiltered(t *testing.T) {
	auth := &cliproxyauth.Auth{ID: "codex-a", Provider: "codex", Metadata: map[string]any{
		"access_token": "access",
		"account_id":   "account",
	}}
	jar := CodexCookieJarForAuth(auth)
	chatURL, _ := url.Parse("https://chatgpt.com/backend-api/wham/usage")
	jar.SetCookies(chatURL, []*http.Cookie{{Name: "__cf_bm", Value: "cf"}, {Name: "chatgpt_session", Value: "secret"}})
	if got := jar.Cookies(chatURL); len(got) != 1 || got[0].Name != "__cf_bm" {
		t.Fatalf("cookies = %#v", got)
	}
	thirdPartyURL, _ := url.Parse("https://api.openai.com/v1/responses")
	if got := jar.Cookies(thirdPartyURL); len(got) != 0 {
		t.Fatalf("third-party cookies = %#v", got)
	}
	InvalidateCodexCookieJar(auth.ID)
}

func TestConfigureCodexChatGPTUsageRequestUsesSystemProfile(t *testing.T) {
	for _, test := range []struct {
		name   string
		system string
		wantUA string
	}{
		{name: "legacy defaults to mac", wantUA: "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) unknown (codex-tui; 0.154.0)"},
		{name: "windows", system: "windows", wantUA: "codex-tui/0.154.0 (Windows 10.0.19044; x86_64) unknown (codex-tui; 0.154.0)"},
	} {
		t.Run(test.name, func(t *testing.T) {
			auth := &cliproxyauth.Auth{
				ID:       "usage-" + test.name,
				Provider: "codex",
				Metadata: map[string]any{"access_token": "access", "account_id": "account", "codex_client_system": test.system},
			}
			req, errNew := http.NewRequest(http.MethodGet, "https://chatgpt.com/backend-api/wham/usage", nil)
			if errNew != nil {
				t.Fatal(errNew)
			}
			req.Header.Set("Cookie", "stale=1")
			if !ConfigureCodexChatGPTUsageRequest(req, auth) {
				t.Fatal("usage request was not recognized")
			}
			if got := req.Header.Get("User-Agent"); got != test.wantUA {
				t.Fatalf("User-Agent = %q, want %q", got, test.wantUA)
			}
			if got := req.Header.Get("Authorization"); got != "Bearer access" {
				t.Fatalf("Authorization = %q", got)
			}
			if got := req.Header.Get("ChatGPT-Account-ID"); got != "account" {
				t.Fatalf("ChatGPT-Account-ID = %q", got)
			}
			if got := req.Header.Get("Cookie"); got != "" {
				t.Fatalf("Cookie = %q, want empty", got)
			}
		})
	}
}

func TestCodexCookieJarsRemainIsolatedAcrossSystemCredentials(t *testing.T) {
	macAuth := &cliproxyauth.Auth{ID: "same-account-mac", Provider: "codex", Metadata: map[string]any{
		"access_token":        "mac-access",
		"codex_client_system": "mac",
	}}
	windowsAuth := &cliproxyauth.Auth{ID: "same-account-windows", Provider: "codex", Metadata: map[string]any{
		"access_token":        "windows-access",
		"codex_client_system": "windows",
	}}
	macJar := CodexCookieJarForAuth(macAuth)
	windowsJar := CodexCookieJarForAuth(windowsAuth)
	chatURL, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	macJar.SetCookies(chatURL, []*http.Cookie{{Name: "__cf_bm", Value: "mac-cookie"}})
	windowsJar.SetCookies(chatURL, []*http.Cookie{{Name: "__cf_bm", Value: "windows-cookie"}})
	if got := macJar.Cookies(chatURL); len(got) != 1 || got[0].Value != "mac-cookie" {
		t.Fatalf("mac cookies = %#v", got)
	}
	if got := windowsJar.Cookies(chatURL); len(got) != 1 || got[0].Value != "windows-cookie" {
		t.Fatalf("windows cookies = %#v", got)
	}
	InvalidateCodexCookieJar(macAuth.ID)
	InvalidateCodexCookieJar(windowsAuth.ID)
}
