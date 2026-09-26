package management

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

func TestOaiLBManagementSaveAndYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("auth-dir: "+dir+"\nport: 8317\n"), 0600); err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: &config.Config{AuthDir: dir}, configFilePath: path}
	invoke := func(body string, fn func(*gin.Context)) *httptest.ResponseRecorder {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest("PUT", "/", strings.NewReader(body))
		c.Request.Header.Set("Content-Type", "application/json")
		fn(c)
		return r
	}
	body := `{"source-instance-id":"nine","source-url":"https://nine.example","source-management-key":"test-source-password","source-auth-id":"auth-nine","source-auth-file":"nine.json"}`
	if r := invoke(body, h.PutCodexOaiLBBorrow); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	raw, _ := os.ReadFile(path)
	if strings.Contains(string(raw), "test-source-password") || !strings.Contains(string(raw), "enc:v1:") {
		t.Fatal("password not encrypted")
	}
	r := invoke("", h.GetCodexOaiLBBorrow)
	if !gjson.GetBytes(r.Body.Bytes(), "supported").Bool() || gjson.GetBytes(r.Body.Bytes(), "config.source-auth-id").String() != "auth-nine" {
		t.Fatal(r.Body.String())
	}
	if r := invoke("{}", h.PutCodexOaiLBBorrow); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	loaded, err := config.LoadConfig(path)
	if err != nil || loaded.CodexHeaderDefaults.OaiLBBorrow != nil {
		t.Fatal("disable failed", err)
	}
	yamlBody := fmt.Sprintf("# keep this\nauth-dir: %s\nport: 8317\ncodex-header-defaults:\n  oailb-borrow:\n    source-url: https://nine.example\n    source-auth-id: auth-nine\n    source-management-key: yaml-password\n", dir)
	if r := invoke(yamlBody, h.PutConfigYAML); r.Code != 200 {
		t.Fatal(r.Body.String())
	}
	raw, _ = os.ReadFile(path)
	if strings.Contains(string(raw), "yaml-password") || !strings.Contains(string(raw), "# keep this") {
		t.Fatal("YAML save lost encryption or comments")
	}
	loaded, err = config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if key, err := config.CodexOaiLBManagementKey(loaded); err != nil || key != "yaml-password" {
		t.Fatal("YAML key did not decrypt", err)
	}
}

func TestOaiLBDonorSelectsExactCredential(t *testing.T) {
	mgr := coreauth.NewManager(nil, nil, nil)
	u, _ := url.Parse("https://chatgpt.com/backend-api/codex/responses")
	values := map[string]string{}
	for _, id := range []string{"selected", "other"} {
		a := &coreauth.Auth{ID: id, Provider: "codex", Metadata: map[string]any{"access_token": "token", "account_id": id}}
		if _, err := mgr.Register(t.Context(), a); err != nil {
			t.Fatal(err)
		}
		body, _ := json.Marshal(map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "host": id})
		values[id] = "e30." + base64.RawURLEncoding.EncodeToString(body) + ".sig"
		helps.CodexCookieJarForAuth(a).SetCookies(u, []*http.Cookie{{Name: "__oailb", Value: values[id], Path: "/"}})
		t.Cleanup(func() { helps.InvalidateCodexCookieJar(id) })
	}
	h := &Handler{cfg: &config.Config{}, authManager: mgr}
	for _, id := range []string{"selected", "missing"} {
		r := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(r)
		c.Request = httptest.NewRequest("POST", "/", strings.NewReader(fmt.Sprintf(`{"auth_id":%q}`, id)))
		c.Request.Header.Set("Content-Type", "application/json")
		h.BorrowCodexOaiLB(c)
		if r.Code != 200 {
			t.Fatal(r.Body.String())
		}
		if id == "selected" && gjson.GetBytes(r.Body.Bytes(), "value").String() != values[id] {
			t.Fatal("wrong credential exported")
		}
		if id == "missing" && gjson.GetBytes(r.Body.Bytes(), "available").Bool() {
			t.Fatal("missing credential fell over to another")
		}
	}
}
