package management

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"os"
	"strings"
	"testing"

	"github.com/MoeForge/moego/base64"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type officeFlowService struct {
	account          string
	entered, release chan struct{}
}

func readSIWCDisk(t *testing.T, path string) map[string]any {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out map[string]any
	if json.Unmarshal(raw, &out) != nil {
		t.Fatal("invalid credential JSON")
	}
	return out
}
func (s *officeFlowService) GenerateBasispointsAuthURL(state string, pkce *codex.PKCECodes) (string, error) {
	return codex.NewCodexAuth(nil).GenerateBasispointsAuthURL(state, pkce)
}
func (s *officeFlowService) ExchangeBasispointsCodeForTokens(context.Context, string, *codex.PKCECodes) (*codex.BasispointsTokenData, error) {
	if s.entered != nil {
		close(s.entered)
		<-s.release
	}
	raw, _ := json.Marshal(map[string]any{"client_id": codex.BasispointsClientID, "aud": codex.BasispointsClientID, "exp": 4102444800, "https://api.openai.com/auth": map[string]any{"chatgpt_account_id": s.account}})
	return &codex.BasispointsTokenData{CodexSIWCTokenData: codex.CodexSIWCTokenData{AccessToken: "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature", IDToken: "header." + base64.RawURLEncoding.EncodeToString(raw) + ".signature", RefreshToken: "office-refresh", ClientID: codex.BasispointsClientID, Expire: "2100-01-01T00:00:00Z"}, AccountID: s.account}, nil
}

func startOfficeFlow(t *testing.T, f *siwcFixture, service *officeFlowService) (string, *siwcBindSession) {
	t.Helper()
	f.h.basispointsOAuthFactory = func(*config.Config) basispointsOAuthService { return service }
	f.router.POST("/office-bind", f.h.StartBasispointsBind)
	f.router.GET("/office-status", f.h.GetBasispointsStatus)
	w := siwcRequest(t, f.router, http.MethodPost, "/office-bind", map[string]any{"name": f.auth.FileName})
	if w.Code != 200 {
		t.Fatalf("start Office: %d %s", w.Code, w.Body.String())
	}
	var response struct{ URL, State, Flow, CallbackMode string }
	if err := json.Unmarshal(w.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	u, _ := url.Parse(response.URL)
	if response.Flow != "basispoints" || u.Query().Get("client_id") != codex.BasispointsClientID || u.Query().Get("scope") != codex.BasispointsScope || u.Query().Get("redirect_uri") != codex.BasispointsRedirectURI {
		t.Fatal("wrong Office authorization contract")
	}
	session, ok := f.store.Get(response.State)
	if !ok || session.SIWC == nil || strings.Contains(w.Body.String(), session.SIWC.pkce.CodeVerifier) {
		t.Fatal("unsafe Office session")
	}
	return response.State, session.SIWC
}

func TestBasispointsBindUsesSeparatePartition(t *testing.T) {
	f := setupSIWCFlow(t, false)
	state, flow := startOfficeFlow(t, f, &officeFlowService{account: "account"})
	callback := codex.BasispointsRedirectURI + "?" + url.Values{"code": {"office-code"}, "state": {state}}.Encode()
	w := siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"provider": "basispoints", "state": state, "redirect_url": callback})
	if w.Code != 200 {
		t.Fatalf("callback: %s", w.Body.String())
	}
	waitSIWCExchange(t, flow)
	disk := readSIWCDisk(t, f.path)
	if disk["access_token"] != "outer" || disk["refresh_token"] != "outer-refresh" || disk["note"] != "keep" || disk["use_basispoints"] != true || disk["siwc"].(map[string]any)["device_id"] == nil {
		t.Fatal("Office binding changed unrelated credentials")
	}
	if disk["basispoints"].(map[string]any)["client_id"] != codex.BasispointsClientID {
		t.Fatal("Office partition missing")
	}
	w = siwcRequest(t, f.router, http.MethodGet, "/office-status?name=codex.json", nil)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"status":"valid"`) || strings.Contains(w.Body.String(), "office-refresh") {
		t.Fatalf("unsafe Office status: %s", w.Body.String())
	}
	if _, ok := f.store.Get(state); !ok {
		t.Fatal("completed session disappeared")
	}
}

func TestBasispointsBindCancellationRejectsLateExchange(t *testing.T) {
	f := setupSIWCFlow(t, false)
	service := &officeFlowService{account: "account", entered: make(chan struct{}), release: make(chan struct{})}
	state, flow := startOfficeFlow(t, f, service)
	w := siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"provider": "basispoints", "state": state, "code": "code"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	<-service.entered
	w = siwcRequest(t, f.router, http.MethodDelete, "/session?state="+state, nil)
	close(service.release)
	waitSIWCExchange(t, flow)
	if _, exists := readSIWCDisk(t, f.path)["basispoints"]; exists {
		t.Fatal("cancelled Office grant was published")
	}
}

func TestBasispointsBindRejectsWrongWorkspaceAndCallback(t *testing.T) {
	f := setupSIWCFlow(t, false)
	state, flow := startOfficeFlow(t, f, &officeFlowService{account: "different-account"})
	w := siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"provider": "basispoints", "state": state, "redirect_url": "https://example.com/auth/callback?code=code&state=" + state})
	if w.Code != 400 {
		t.Fatal("accepted foreign callback origin")
	}
	w = siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"provider": "basispoints", "state": state, "code": "code", "client_id": codex.ClientID})
	if w.Code != 400 {
		t.Fatal("accepted Codex client as Office client")
	}
	w = siwcRequest(t, f.router, http.MethodPost, "/callback", map[string]any{"provider": "basispoints", "state": state, "code": "code"})
	if w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	waitSIWCExchange(t, flow)
	if _, exists := readSIWCDisk(t, f.path)["basispoints"]; exists {
		t.Fatal("bound another workspace")
	}
}
