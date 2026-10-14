package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api/handlers/management"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

const oailbContractSettings = "/v0/management/codex-oailb-borrow"
const oailbContractBorrow = "/v0/management/codex/oailb/borrow"
const oailbContractKey = "local-contract-management-key"

type oailbContractFixture struct {
	server  *Server
	handler *management.Handler
	path    string
	cfg     *config.Config
	manager *coreauth.Manager
}

func newOailbContractFixture(t *testing.T, v8 bool) oailbContractFixture {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	raw := fmt.Sprintf("# preserved contract fixture\nauth-dir: %s\nport: 8317\nremote-management: {secret-key: %s}\ncodex:\n  turn-state-ticket: {enabled: true}\n", dir, oailbContractKey)
	if v8 {
		raw = fmt.Sprintf("config-version: 8\n# preserved contract fixture\nserver: {port: 8317}\nmanagement: {secret-key: %s}\noauth:\n  auth-dir: %s\n  providers:\n    codex:\n      turn-state-ticket: {enabled: true}\n", oailbContractKey, dir)
	}
	if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	manager := coreauth.NewManager(nil, nil, nil)
	h := management.NewHandler(cfg, path, manager)
	h.SetLocalPassword(oailbContractKey)
	s := &Server{cfg: cfg, engine: gin.New(), mgmt: h}
	s.managementRoutesEnabled.Store(true)
	s.registerManagementRoutes()
	return oailbContractFixture{server: s, handler: h, path: path, cfg: cfg, manager: manager}
}
func (f oailbContractFixture) request(t *testing.T, method, path, body string, authorized bool, status int) []byte {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.RemoteAddr = "127.0.0.1:54321"
	req.Header.Set("Content-Type", "application/json")
	if authorized {
		req.Header.Set("Authorization", "Bearer "+oailbContractKey)
	}
	rec := httptest.NewRecorder()
	f.server.engine.ServeHTTP(rec, req)
	if rec.Code != status {
		t.Fatalf("%s %s: status=%d want=%d body=%s", method, path, rec.Code, status, rec.Body.String())
	}
	if status == 200 && strings.HasPrefix(path, oailbContractSettings) && rec.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("settings response is cacheable")
	}
	return rec.Body.Bytes()
}
func oailbContractState(now time.Time, nonce byte) string {
	raw := make([]byte, 585)
	raw[0] = 128
	binary.BigEndian.PutUint64(raw[1:9], uint64(now.Unix()))
	raw[9] = nonce
	return base64.URLEncoding.EncodeToString(raw)
}

type oailbContractTransport func(*http.Request) (*http.Response, error)

func (f oailbContractTransport) RoundTrip(req *http.Request) (*http.Response, error) { return f(req) }

// Exercise the real production routes, middleware, credential listing and disk
// persistence in the exact order used by CPAMP, not hand-written mock handlers.
func TestOaiLBOriginalCPAMPContractAndLiveBorrow(t *testing.T) {
	for _, v8 := range []bool{false, true} {
		t.Run(fmt.Sprintf("v8=%v", v8), func(t *testing.T) {
			source := newOailbContractFixture(t, v8)
			target := newOailbContractFixture(t, v8)
			now := time.Now().Truncate(time.Second)
			seed := oailbContractState(now.Add(-50*time.Minute), 1)
			for _, system := range []string{"mac", "windows"} {
				filename := "codex-source-" + system + ".json"
				path := filepath.Join(source.cfg.AuthDir, filename)
				if err := os.WriteFile(path, []byte(`{"type":"codex"}`), 0600); err != nil {
					t.Fatal(err)
				}
				auth := &coreauth.Auth{ID: "source-" + system, Provider: "codex", FileName: filename, Attributes: map[string]string{"path": path}, Metadata: map[string]any{"access_token": "source-oauth-never-export", "email": "source@example.test", "account_id": "shared-account", "codex_client_system": system}}
				route := helps.CodexTicketRoute{CFLB: helps.CodexRouteCookie{Value: system + "-cf"}, OaiLB: helps.CodexRouteCookie{Value: system + "-oai"}}
				helps.StoreCodexTurnStateTicket(auth, helps.CodexTurnStateTicket{Model: "gpt-6-astra", Owner: helps.CodexOwnerFingerprint(auth), State: seed, CapturedAt: now.Add(-50 * time.Minute), ExpiresAt: now.Add(10 * time.Minute), Route: &route})
				if _, err := source.manager.Register(t.Context(), auth); err != nil {
					t.Fatal(err)
				}
			}
			for _, f := range []oailbContractFixture{source, target} {
				raw := f.request(t, "GET", oailbContractSettings, "", true, 200)
				if !gjson.GetBytes(raw, "supported").Bool() || gjson.GetBytes(raw, "config").Type != gjson.Null {
					t.Fatal("unconfigured API was reported unsupported")
				}
				f.request(t, "GET", oailbContractSettings, "", false, 401)
				f.request(t, "PUT", oailbContractSettings, "{}", false, 401)
				f.request(t, "POST", oailbContractBorrow, `{"auth_id":"source-windows","model":"gpt-6-astra"}`, false, 401)
				f.request(t, "POST", "/v8/management/credentials/codex/ticket-borrow", `{}`, true, 404)
			}
			credentials := source.request(t, "GET", "/v0/management/auth-files", "", true, 200)
			files := gjson.GetBytes(credentials, "files").Array()
			if len(files) != 2 {
				t.Fatalf("source selector expected two credentials: %s", credentials)
			}
			for _, file := range files {
				if file.Get("id").String() == "" || file.Get("name").String() == "" || file.Get("provider").String() != "codex" || file.Get("account_type").String() != "oauth" || file.Get("runtime_only").Bool() || file.Get("disabled").Bool() {
					t.Fatalf("CPAMP could not select credential: %s", file.Raw)
				}
			}
			donor := httptest.NewServer(source.server.engine)
			defer donor.Close()
			payload := map[string]string{"source-instance-id": "source-instance", "source-url": donor.URL, "source-management-key": oailbContractKey, "source-auth-id": "source-windows", "source-auth-file": "codex-source-windows.json"}
			body, _ := json.Marshal(payload)
			saved := target.request(t, "PUT", oailbContractSettings, string(body), true, 200)
			if strings.TrimSpace(string(saved)) != `{"status":"ok"}` {
				t.Fatal("save response changed the original envelope")
			}
			read := target.request(t, "GET", oailbContractSettings, "", true, 200)
			if !gjson.GetBytes(read, "supported").Bool() || gjson.GetBytes(read, "config.source-auth-id").String() != "source-windows" || gjson.GetBytes(read, "config.source-instance-id").String() != "source-instance" || gjson.GetBytes(read, "config.source-auth-file").String() != "codex-source-windows.json" {
				t.Fatalf("saved selection was not returned: %s", read)
			}
			encrypted := gjson.GetBytes(read, "config.source-management-key").String()
			if !strings.HasPrefix(encrypted, "enc:v1:") || strings.Contains(string(read), oailbContractKey) {
				t.Fatal("source secret was not protected")
			}
			disk, _ := os.ReadFile(target.path)
			if strings.Contains(string(disk), oailbContractKey) || !strings.Contains(string(disk), "preserved contract fixture") || strings.Contains(string(disk), "config-version: 8") != v8 {
				t.Fatal("save lost layout, comments or encryption")
			}
			loaded, err := config.LoadConfig(target.path)
			if err != nil {
				t.Fatal(err)
			}
			if key, err := config.CodexOaiLBManagementKey(loaded); err != nil || key != oailbContractKey {
				t.Fatal("saved donor secret cannot be used", err)
			}
			target.handler.SetConfig(loaded)
			// The actual borrower reads the actual donor route, then validates with its own identity.
			borrower := &coreauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "target-oauth", "account_id": "target-account"}}
			defer helps.InvalidateCodexCookieJar(borrower.ID)
			newState := oailbContractState(now, 2)
			var shots atomic.Int32
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", oailbContractTransport(func(req *http.Request) (*http.Response, error) {
				n := shots.Add(1)
				if req.Header.Get("Authorization") != "Bearer target-oauth" {
					t.Error("target probe used the wrong identity")
				}
				for name, want := range map[string]string{"__cflb": "windows-cf", "__oailb": "windows-oai"} {
					cookie, err := req.Cookie(name)
					if err != nil || cookie.Value != want {
						t.Errorf("incorrect %s pairing", name)
					}
				}
				headers := make(http.Header)
				if n == 1 {
					if req.Header.Get(helps.CodexTurnStateTicketHeader) != seed {
						t.Error("wrong donor seed")
					}
					headers.Set(helps.CodexTurnStateTicketHeader, newState)
				} else if req.Header.Get(helps.CodexTurnStateTicketHeader) != newState {
					t.Error("wrong continuation ticket")
				}
				return &http.Response{StatusCode: 200, Header: headers, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n")), Request: req}, nil
			}))
			borrowed, err := helps.AcquireCodexBorrowedTicket(ctx, loaded, borrower, "gpt-6-astra")
			if err != nil || borrowed == nil || borrowed.State != newState || shots.Load() != 2 {
				t.Fatal("live donor and borrower did not interoperate", err)
			}
			unavailable := source.request(t, "POST", oailbContractBorrow, `{"auth_id":"source-windows","model":"missing-model"}`, true, 200)
			if string(unavailable) != `{"available":false}` {
				t.Fatal("unavailable response changed original envelope")
			}
			source.request(t, "POST", oailbContractBorrow, `{"auth_id":"source-windows"}`, true, 400)
			// The v8 generic writer and CPAMP's original endpoint must operate on one value.
			v8Path := "/v8/management/config/oauth/providers/codex/header-defaults/oailb-borrow"
			v8Read := target.request(t, "GET", v8Path, "", true, 200)
			if gjson.GetBytes(v8Read, "source-management-key").String() != encrypted {
				t.Fatal("v8 view diverged from CPAMP settings")
			}
			payload["source-auth-id"] = "source-mac"
			payload["source-auth-file"] = "codex-source-mac.json"
			body, _ = json.Marshal(payload)
			target.request(t, "PUT", v8Path, string(body), true, 200)
			read = target.request(t, "GET", oailbContractSettings, "", true, 200)
			if gjson.GetBytes(read, "config.source-auth-id").String() != "source-mac" {
				t.Fatal("v8 update did not reach original endpoint")
			}
			target.request(t, "PUT", oailbContractSettings, "{}", true, 200)
			read = target.request(t, "GET", oailbContractSettings, "", true, 200)
			if string(read) != `{"config":null,"supported":true}` {
				t.Fatal("clear did not preserve supported capability")
			}
			cleared, err := config.LoadConfig(target.path)
			if err != nil || cleared.CodexHeaderDefaults.OaiLBBorrow != nil {
				t.Fatal("clear did not persist", err)
			}
			target.request(t, "PUT", oailbContractSettings, "null", true, 200)
			after, _ := os.ReadFile(target.path)
			target.request(t, "PUT", oailbContractSettings, `{"source-url":"http://incomplete.example"}`, true, 400)
			target.request(t, "PUT", oailbContractSettings, `{"unsupported":"must-not-clear"}`, true, 400)
			rejected, _ := os.ReadFile(target.path)
			if !bytes.Equal(after, rejected) {
				t.Fatal("rejected save changed persisted configuration")
			}
		})
	}
}

func TestOaiLBSupportedIndependentOfTicketReadiness(t *testing.T) {
	f := newOailbContractFixture(t, true)
	f.cfg.Codex.TurnStateTicket.Enabled = false
	raw := f.request(t, "GET", oailbContractSettings, "", true, 200)
	if !gjson.GetBytes(raw, "supported").Bool() {
		t.Fatal("disabled ticket switch hid API support")
	}
	raw = f.request(t, "POST", oailbContractBorrow, `{"auth_id":"missing","model":"gpt-6-astra"}`, true, 200)
	if string(raw) != `{"available":false}` {
		t.Fatal("disabled collection exported source material")
	}
}
