package codex

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/MoeForge/moego/base64"
)

type officeTestTransport func(*http.Request) (*http.Response, error)

func (f officeTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func officeTestJWT(client, account, user string, expiry int64) string {
	raw, _ := json.Marshal(map[string]any{"client_id": client, "aud": client, "exp": expiry, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": account, "chatgpt_user_id": user}})
	return "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature"
}

func TestBasispointsOAuthContract(t *testing.T) {
	pkce := &PKCECodes{CodeVerifier: "verifier", CodeChallenge: "challenge"}
	service := NewCodexAuth(nil)
	authorize, err := service.GenerateBasispointsAuthURL("state", pkce)
	if err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(authorize)
	if u.Path != "/api/accounts/authorize" || u.Query().Get("client_id") != BasispointsClientID || u.Query().Get("scope") != BasispointsScope || u.Query().Get("redirect_uri") != BasispointsRedirectURI || u.Query().Get("audience") != "https://api.openai.com/v1" || u.Query().Get("code_challenge_method") != "S256" {
		t.Fatalf("wrong Office authorization URL: %s", authorize)
	}
	calls := 0
	service.httpClient = &http.Client{Transport: officeTestTransport(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.String() != BasispointsTokenURL || r.Method != http.MethodPost {
			t.Fatal("wrong Office token endpoint")
		}
		if errParse := r.ParseForm(); errParse != nil {
			t.Fatal(errParse)
		}
		if r.Form.Get("client_id") != BasispointsClientID || r.Form.Has("scope") {
			t.Fatal("Office request used Codex client or refresh scope")
		}
		refresh := "rotated-office-refresh"
		if calls == 1 {
			if r.Form.Get("redirect_uri") != BasispointsRedirectURI || r.Form.Get("code_verifier") != "verifier" {
				t.Fatal("wrong PKCE exchange")
			}
		} else {
			if r.Form.Get("grant_type") != "refresh_token" || r.Form.Get("refresh_token") != refresh {
				t.Fatal("wrong Office refresh")
			}
			refresh = ""
		}
		body, _ := json.Marshal(map[string]any{"access_token": officeTestJWT(BasispointsClientID, "account", "user", 4102444800), "id_token": officeTestJWT(BasispointsClientID, "account", "user", 4102444800), "refresh_token": refresh, "scope": BasispointsScope})
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(string(body))), Header: make(http.Header)}, nil
	})}
	grant, err := service.ExchangeBasispointsCodeForTokens(context.Background(), "code", pkce)
	if err != nil {
		t.Fatal(err)
	}
	refreshed, err := service.RefreshBasispointsTokens(context.Background(), grant.ToMap())
	if err != nil || refreshed.RefreshToken != "rotated-office-refresh" || refreshed.AccountID != "account" || refreshed.UserID != "user" {
		t.Fatalf("bad Office refresh: %v", err)
	}
}

func TestBasispointsGrantRejectsCodexAndIdentityChanges(t *testing.T) {
	good := map[string]any{"client_id": BasispointsClientID, "access_token": officeTestJWT(BasispointsClientID, "account", "user", 4102444800), "id_token": officeTestJWT(BasispointsClientID, "account", "user", 4102444800), "refresh_token": "office-refresh", "account_id": "account"}
	if _, _, _, err := ValidateBasispointsGrant(good, time.Unix(100, 0), false); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, key string
		value     any
	}{
		{"wrong_client", "client_id", ClientID},
		{"codex_token", "access_token", officeTestJWT(ClientID, "account", "user", 4102444800)},
		{"workspace", "account_id", "another-account"},
		{"expired", "access_token", officeTestJWT(BasispointsClientID, "account", "user", 99)},
		{"missing_refresh", "refresh_token", ""},
		{"missing_identity", "id_token", ""},
		{"wrong_identity_client", "id_token", officeTestJWT(ClientID, "account", "user", 4102444800)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			grant := map[string]any{}
			for k, v := range good {
				grant[k] = v
			}
			grant[tc.key] = tc.value
			if _, _, _, err := ValidateBasispointsGrant(grant, time.Unix(100, 0), false); err == nil {
				t.Fatal("accepted invalid Office grant")
			}
		})
	}
	outer := map[string]any{"account_id": "account", "access_token": officeTestJWT(ClientID, "account", "different-user", 4102444800)}
	if err := ValidateBasispointsIdentity(good, outer); err == nil {
		t.Fatal("accepted another member of the same workspace")
	}
}

func TestBasispointsTokenErrorsDoNotExposeSecrets(t *testing.T) {
	service := NewCodexAuth(nil)
	service.httpClient = &http.Client{Transport: officeTestTransport(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 400, Body: io.NopCloser(strings.NewReader(`{"error":"invalid_grant","access_token":"TOP-SECRET"}`))}, nil
	})}
	_, err := service.ExchangeBasispointsCodeForTokens(context.Background(), "SECRET-CODE", &PKCECodes{CodeVerifier: "SECRET-VERIFIER"})
	if err == nil || strings.Contains(err.Error(), "SECRET") {
		t.Fatalf("unsafe error: %v", err)
	}
}

func TestBasispointsPartitionSurvivesOtherCredentialSaves(t *testing.T) {
	disk := map[string]any{"access_token": "new-outer", "refresh_token": "new-refresh", "basispoints": map[string]any{"access_token": "new-office", "refresh_token": "rotated-office"}, "use_basispoints": true, "siwc": map[string]any{"access_token": "siwc"}}
	stale := map[string]any{"access_token": "old-outer", "basispoints": map[string]any{"access_token": "old-office"}}
	for _, merged := range []map[string]any{MergeWithDiskMap(disk, stale), MergeCodexCredentialsFromDisk(disk, stale)} {
		if merged["basispoints"].(map[string]any)["access_token"] != "new-office" {
			t.Fatal("stale save replaced Office grant")
		}
	}
	stale["use_basispoints"] = false
	if merged := MergeCodexCredentialsFromDisk(disk, stale); merged["use_basispoints"] != false {
		t.Fatal("explicit Office switch was lost")
	}
}
