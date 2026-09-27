package management

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexRuntimeManagementCannotMutateCredentials(t *testing.T) {
	m := coreauth.NewManager(nil, nil, nil)
	a := &coreauth.Auth{ID: "codex-runtime:worker", Provider: coreauth.CodexRuntimeProvider, Status: coreauth.StatusActive}
	if _, err := m.Register(context.Background(), a); err != nil {
		t.Fatal(err)
	}
	h := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: t.TempDir()}, m)
	for _, tc := range []struct {
		name, method, path, body string
		handler                  func(*gin.Context)
	}{
		{"delete", http.MethodDelete, "/auth-files?name=" + url.QueryEscape(a.ID), "", h.DeleteAuthFile},
		{"refresh", http.MethodPost, "/auth-files/refresh?name=" + url.QueryEscape(a.ID), "", h.RefreshAuthFiles},
		{"disable", http.MethodPatch, "/auth-files/status", `{"name":"codex-runtime:worker","disabled":true}`, h.PatchAuthFileStatus},
		{"fields", http.MethodPatch, "/auth-files/fields", `{"name":"codex-runtime:worker","prefix":"new"}`, h.PatchAuthFileFields},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(rec)
			ctx.Request = httptest.NewRequest(tc.method, tc.path, strings.NewReader(tc.body))
			ctx.Request.Header.Set("Content-Type", "application/json")
			tc.handler(ctx)
			if rec.Code != http.StatusConflict {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
		})
	}
	if got, ok := m.GetByID(a.ID); !ok || got.Disabled || got.Prefix != "" {
		t.Fatal("management changed config-owned runtime")
	}
	if _, err := h.buildAuthFromFileData("/tmp/runtime-injection.json", []byte(`{"type":" CODEX-RUNTIME ","access_token":"synthetic"}`)); err == nil {
		t.Fatal("runtime credential injection accepted")
	}
}
