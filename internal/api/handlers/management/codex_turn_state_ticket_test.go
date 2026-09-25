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
		for _, field := range []string{"target_length", "personal_target_length", "team_target_length"} {
			if gjson.GetBytes(rec.Body.Bytes(), field).Int() != 780 {
				t.Fatalf("%s must use the fixed 780-byte shape", field)
			}
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

func TestCodexTicketManagementFixedTargetLength(t *testing.T) {
	h := &Handler{cfg: &config.Config{}, configFilePath: writeTestConfigFile(t)}
	for _, tc := range []struct {
		body   string
		status int
	}{
		{`{"target_length":780}`, http.StatusOK},
		{`{"target_length":292}`, http.StatusBadRequest},
		{`{"target_length":332}`, http.StatusBadRequest},
	} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/codex-turn-state-ticket", strings.NewReader(tc.body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PutCodexTurnStateTicket(ctx)
		if rec.Code != tc.status {
			t.Fatalf("%s returned %d, want %d", tc.body, rec.Code, tc.status)
		}
		cfg, errLoad := config.LoadConfig(h.configFilePath)
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		if cfg.Codex.TurnStateTicket.TargetLength != 780 {
			t.Fatal("management did not persist the fixed length")
		}
	}
}

func TestCodexTicketManagementIndependentOfBasispoints(t *testing.T) {
	h := &Handler{cfg: &config.Config{Codex: config.CodexConfig{
		Basispoints:    config.CodexBasispointsConfig{Enabled: true},
		ForceWebsocket: true,
		TurnStateTicket: config.CodexTurnStateTicketConfig{
			Enabled: true, ProbeIntervalSeconds: 90, TTLSeconds: 1800,
			Models: []string{"gpt-6-astra"},
		},
	}}, configFilePath: writeTestConfigFile(t)}
	get := func(wantEnabled bool, wantInterval int64) {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodGet, "/v0/management/codex-turn-state-ticket", nil)
		h.GetCodexTurnStateTicket(ctx)
		body := rec.Body.Bytes()
		if rec.Code != http.StatusOK || gjson.GetBytes(body, "enabled").Bool() != wantEnabled {
			t.Fatalf("ticket enabled must remain independent of Basispoints: %s", body)
		}
		if gjson.GetBytes(body, "probe_interval_seconds").Int() != wantInterval ||
			gjson.GetBytes(body, "ttl_seconds").Int() != 1800 ||
			gjson.GetBytes(body, "models.0").String() != "gpt-6-astra" ||
			!gjson.GetBytes(body, "accounts").IsArray() {
			t.Fatalf("ticket settings or status missing: %s", body)
		}
	}
	get(true, 90)
	for _, enabled := range []bool{false, true} {
		body := `{"enabled":false,"probe_interval_seconds":120}`
		if enabled {
			body = `{"enabled":true,"probe_interval_seconds":120}`
		}
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPatch, "/v0/management/codex-turn-state-ticket", strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PutCodexTurnStateTicket(ctx)
		if rec.Code != http.StatusOK {
			t.Fatalf("update status %d: %s", rec.Code, rec.Body.String())
		}
		saved, errLoad := config.LoadConfig(h.configFilePath)
		if errLoad != nil {
			t.Fatal(errLoad)
		}
		if !saved.Codex.Basispoints.Enabled || !saved.Codex.ForceWebsocket ||
			saved.Codex.EffectiveTurnStateTicket().Enabled != enabled {
			t.Fatal("ticket update overwrote an independent Codex setting")
		}
		h.cfg = saved
		get(enabled, 120)
	}
}
