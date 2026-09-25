package management

import (
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"gopkg.in/yaml.v3"
)

func TestCodexForceWebsocketSettingPersistsIndependently(t *testing.T) {
	h := &Handler{cfg: &config.Config{WebsocketAuth: true}, configFilePath: writeTestConfigFile(t)}
	for _, value := range []string{"true", "false"} {
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPut, "/v0/management/codex/force-websocket", strings.NewReader(`{"value":`+value+`}`))
		ctx.Request.Header.Set("Content-Type", "application/json")
		h.PutCodexForceWebsocket(ctx)
		if rec.Code != 200 {
			t.Fatalf("status=%d: %s", rec.Code, rec.Body.String())
		}
		data, err := os.ReadFile(h.configFilePath)
		if err != nil {
			t.Fatal(err)
		}
		var saved config.Config
		if errDecode := yaml.Unmarshal(data, &saved); errDecode != nil {
			t.Fatal(errDecode)
		}
		if saved.Codex.ForceWebsocket != (value == "true") || !h.cfg.WebsocketAuth {
			t.Fatalf("setting or auth changed incorrectly: %s", data)
		}
	}
}
