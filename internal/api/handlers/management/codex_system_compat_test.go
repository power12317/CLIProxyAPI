package management

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

func TestCodexCredentialFileNameForSystemKeepsMacLegacyName(t *testing.T) {
	base := "codex-abc-user@example.com-pro.json"
	if got := codexCredentialFileNameForSystem(base, "mac"); got != base {
		t.Fatalf("mac filename = %q, want %q", got, base)
	}
	if got := codexCredentialFileNameForSystem("codex-abc-user@example.com-pro-mac.json", "mac"); got != base {
		t.Fatalf("legacy mac filename = %q, want %q", got, base)
	}
	if got := codexCredentialFileNameForSystem(base, "windows"); got != "codex-abc-user@example.com-pro-windows.json" {
		t.Fatalf("windows filename = %q", got)
	}
}

func TestCodexClientSystemForAuthDefaultsToMac(t *testing.T) {
	if got := codexClientSystemForAuth(&coreauth.Auth{Provider: "codex"}); got != "mac" {
		t.Fatalf("missing system = %q, want mac", got)
	}
	if got := codexClientSystemForAuth(&coreauth.Auth{Provider: "codex", Metadata: map[string]any{"codex_client_system": "windows"}}); got != "windows" {
		t.Fatalf("windows system = %q", got)
	}
}

func TestGetCodexCapabilities(t *testing.T) {
	gin.SetMode(gin.TestMode)
	h := &Handler{}
	router := gin.New()
	router.GET("/codex-capabilities", h.GetCodexCapabilities)
	req := httptest.NewRequest(http.MethodGet, "/codex-capabilities", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if body := rec.Body.String(); body != `{"system_scoped_oauth":true}` {
		t.Fatalf("body = %q", body)
	}
}
