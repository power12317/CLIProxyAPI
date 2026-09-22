package management

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestCodexTicketCacheAllModelsManagementRoundTrip(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, configFilePath: writeTestConfigFile(t)}
	get := func(want bool) {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/codex-turn-state-ticket", nil)
		h.GetCodexTurnStateTicket(ctx)
		value := gjson.GetBytes(rec.Body.Bytes(), "cache_all_models")
		if rec.Code != http.StatusOK || !value.Exists() || value.Bool() != want {
			t.Fatalf("GET cache_all_models = %s, want %v (status %d)", value.Raw, want, rec.Code)
		}
	}
	get(true)
	for _, tc := range []struct {
		method, body string
		want         bool
	}{
		{http.MethodPatch, `{"cache_all_models":false}`, false},
		{http.MethodPut, `{"enabled":true}`, false},
		{http.MethodPatch, `{"cache_all_models":true}`, true},
	} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(tc.method, "/v0/management/codex-turn-state-ticket", strings.NewReader(tc.body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PutCodexTurnStateTicket(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("update status %d: %s", rec.Code, rec.Body.String())
		}
		cfg, errLoad := config.LoadConfig(h.configFilePath)
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		if cfg.Codex.TurnStateTicket.CacheAllModelsEnabled() != tc.want {
			t.Fatalf("management update %s did not persist cache_all_models = %v", tc.body, tc.want)
		}
		get(tc.want)
	}
}
