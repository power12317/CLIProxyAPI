package management

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestOaiLBSettingsFailedSavePreservesRuntimeAndFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte("auth-dir: "+dir+"\nport: 8317\n"), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.LoadConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	h := &Handler{cfg: cfg, configFilePath: path}
	router := gin.New()
	router.PUT("/settings", h.PutCodexOaiLBBorrow)
	router.GET("/settings", h.GetCodexOaiLBBorrow)
	call := func(method, body string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, httptest.NewRequest(method, "/settings", strings.NewReader(body)))
		return rec
	}
	body := `{"source-instance-id":"a","source-url":"http://127.0.0.1:8318","source-management-key":"plaintext-never-leak","source-auth-id":"windows","source-auth-file":"windows.json"}`
	if rec := call(http.MethodPut, body); rec.Code != 200 {
		t.Fatal(rec.Code, rec.Body.String())
	}
	previous := h.cfg
	baseline, _ := os.ReadFile(path)
	for _, invalid := range []string{`{"source-url":"file:///private"}`, `{"source-url":"http://127.0.0.1","source-auth-id":"a","source-management-key":"enc:v1:bad:bad"}`, `{} {}`, `[]`, `{"unknown":"must-not-clear"}`} {
		if rec := call(http.MethodPut, invalid); rec.Code != 400 {
			t.Fatal("invalid request was accepted", rec.Code)
		}
		current, _ := os.ReadFile(path)
		if h.cfg != previous || !bytes.Equal(current, baseline) {
			t.Fatal("invalid update changed active or persisted config")
		}
	}
	h.configFilePath = filepath.Join(dir, "missing", "config.yaml")
	if rec := call(http.MethodPut, `{}`); rec.Code != 500 {
		t.Fatal("failed persistence reported success", rec.Code)
	}
	if h.cfg != previous {
		t.Fatal("failed clear changed active config")
	}
	if rec := call(http.MethodGet, ""); strings.Contains(rec.Body.String(), "plaintext-never-leak") || !strings.Contains(rec.Body.String(), `"supported":true`) {
		t.Fatal("GET lost support or exposed plaintext")
	}
}

func TestOaiLBSettingsDoesNotExposeInProcessPlaintext(t *testing.T) {
	h := &Handler{cfg: &config.Config{CodexHeaderDefaults: config.CodexHeaderDefaults{OaiLBBorrow: &config.CodexOaiLBBorrowConfig{SourceURL: "http://127.0.0.1", SourceAuthID: "a", SourceManagementKey: "in-process-secret"}}}}
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	c.Request = httptest.NewRequest(http.MethodGet, "/settings", nil)
	h.GetCodexOaiLBBorrow(c)
	if rec.Code != 200 || strings.Contains(rec.Body.String(), "in-process-secret") || h.cfg.CodexHeaderDefaults.OaiLBBorrow.SourceManagementKey != "in-process-secret" {
		t.Fatal("GET exposed or mutated an in-process secret")
	}
}
