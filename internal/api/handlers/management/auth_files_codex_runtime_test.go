package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

func TestCodexRuntimeSharedFileAllowsNormalManagement(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "original.json")
	metadata := map[string]any{"type": "codex", "access_token": "latest", "codex_cli": map[string]any{"enabled": true}, "extension": true}
	if err := codexshared.Write(path, metadata); err != nil {
		t.Fatal(err)
	}
	m := coreauth.NewManager(nil, nil, nil)
	a := &coreauth.Auth{ID: "original.json", FileName: "original.json", Provider: "codex", Status: coreauth.StatusActive, Metadata: metadata, Attributes: map[string]string{coreauth.AttributePath: path}}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: dir}, m)
	for _, tc := range []struct {
		name, method, path, body string
		handler                  func(*gin.Context)
		status                   int
	}{
		{"refresh", http.MethodPost, "/auth-files/refresh?name=" + url.QueryEscape(a.ID), "", h.RefreshAuthFiles, http.StatusConflict},
		{"fields", http.MethodPatch, "/auth-files/fields", `{"name":"original.json","prefix":"new"}`, h.PatchAuthFileFields, http.StatusOK},
		{"disable", http.MethodPatch, "/auth-files/status", `{"name":"original.json","disabled":true}`, h.PatchAuthFileStatus, http.StatusOK},
		{"delete", http.MethodDelete, "/auth-files?name=" + url.QueryEscape(a.ID), "", h.DeleteAuthFile, http.StatusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			tc.handler(ctx)
			if rec.Code != tc.status {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			if tc.name == "fields" || tc.name == "disable" {
				got, err := codexshared.Read(path)
				if err != nil || got["access_token"] != "latest" || got["extension"] != true {
					t.Fatal(got, err)
				}
			}
		})
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("original file not deleted", err)
	}
}
