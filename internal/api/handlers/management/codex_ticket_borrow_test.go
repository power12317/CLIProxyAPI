package management

import (
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexTicketBorrowEndpointReadOnlySnapshot(t *testing.T) {
	gin.SetMode(gin.TestMode)
	now := time.Now().Truncate(time.Second)
	raw := make([]byte, 585)
	raw[0] = 128
	binary.BigEndian.PutUint64(raw[1:9], uint64(now.Add(-50*time.Minute).Unix()))
	state := base64.URLEncoding.EncodeToString(raw)
	auth := &cliproxyauth.Auth{ID: "donor", Provider: "codex", Metadata: map[string]any{"access_token": "oauth-secret", "account_id": "account"}}
	route := helps.CodexTicketRoute{CFLB: helps.CodexRouteCookie{Value: "cf"}, OaiLB: helps.CodexRouteCookie{Value: "oai"}}
	helps.StoreCodexTurnStateTicket(auth, helps.CodexTurnStateTicket{Model: "model", Owner: helps.CodexOwnerFingerprint(auth), State: state, Route: &route, CapturedAt: now.Add(-50 * time.Minute), ExpiresAt: now.Add(10 * time.Minute)})
	manager := cliproxyauth.NewManager(nil, nil, nil)
	if _, err := manager.Register(t.Context(), auth); err != nil {
		t.Fatal(err)
	}
	h := &Handler{authManager: manager, cfg: &config.Config{Codex: config.CodexConfig{TurnStateTicket: config.CodexTurnStateTicketConfig{Enabled: true}}}}
	router := gin.New()
	router.POST(helps.CodexTicketBorrowPath, h.BorrowCodexTicket)
	for range 2 {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, helps.CodexTicketBorrowPath, strings.NewReader(`{"auth_id":"donor","model":"model"}`)))
		if rec.Code != 200 || rec.Header().Get("Cache-Control") != "no-store" || strings.Contains(rec.Body.String(), "oauth-secret") {
			t.Fatalf("bad source export: %d", rec.Code)
		}
		var got helps.CodexTurnStateTicket
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatal(err)
		}
		if got.State != state || !got.ExpiresAt.Equal(now.Add(10*time.Minute)) || got.Route.CFLB.Value != "cf" {
			t.Fatal("export renewed or changed source material")
		}
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, helps.CodexTicketBorrowPath, strings.NewReader(`{"auth_id":"donor","model":"unavailable"}`)))
	if rec.Code != 503 {
		t.Fatal("missing model was not reported")
	}
}

func TestConfigV8BorrowSecretProtectedAndNullable(t *testing.T) {
	gin.SetMode(gin.TestMode)
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	initial := "config-version: 8\n# preserved\noauth:\n  auth-dir: " + dir + "\nrequests:\n  payload:\n    default: null\n"
	if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/v8/management/config/*path", h.ConfigV8)
	router.GET("/v8/management/config/*path", h.ConfigV8)
	router.DELETE("/v8/management/config/*path", h.ConfigV8)
	endpoint := "/v8/management/config/oauth/providers/codex/header-defaults/oailb-borrow"
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, endpoint, strings.NewReader(`{"source-url":"http://127.0.0.1:8318","source-auth-id":"donor","source-management-key":"donor-plaintext"}`)))
	if rec.Code != 200 {
		t.Fatalf("config update: %d %s", rec.Code, rec.Body.String())
	}
	saved, _ := os.ReadFile(path)
	if strings.Contains(string(saved), "donor-plaintext") || !strings.Contains(string(saved), "enc:v1:") || !strings.Contains(string(saved), "default: null") || !strings.Contains(string(saved), "# preserved") {
		t.Fatalf("key leaked or unrelated YAML changed: %s", saved)
	}
	loaded, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if key, err := config.CodexOaiLBManagementKey(loaded); err != nil || key != "donor-plaintext" {
		t.Fatal("saved key unavailable", err)
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, endpoint, nil))
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "donor-plaintext") {
		t.Fatal("plaintext exposed through configuration GET")
	}
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodPut, endpoint, strings.NewReader(`null`)))
	if rec.Code != 200 || h.cfg.CodexHeaderDefaults.OaiLBBorrow != nil {
		t.Fatal("null did not disable borrowing")
	}
}
