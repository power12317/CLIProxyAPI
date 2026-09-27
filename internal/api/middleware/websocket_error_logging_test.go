package middleware

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

func TestRecordedBasispointsFailureSurvivesClientCancellation(t *testing.T) {
	for _, status := range []int{http.StatusOK, 499} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			dir := t.TempDir()
			router := gin.New()
			router.Use(RequestLoggingMiddleware(logging.NewFileRequestLogger(false, dir, "", 10)))
			router.POST("/v1/responses", func(c *gin.Context) {
				ctx, cancel := context.WithCancel(c.Request.Context())
				c.Request = c.Request.WithContext(ctx)
				ctx = context.WithValue(ctx, "gin", c)
				helps.RecordBasispointsFailure(ctx, &config.Config{}, []byte(`{"input":"original"}`), []byte(`{"input":"wire"}`), []byte(`{"type":"response.completed"}`), "response_conversion", errors.New("invalid_tool_envelope"))
				c.Status(status)
				cancel()
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"input":"original"}`)))
			files, err := filepath.Glob(filepath.Join(dir, "*.log"))
			if err != nil || len(files) != 1 {
				t.Fatalf("recorded failure was discarded after cancellation: files=%v err=%v", files, err)
			}
			content, err := os.ReadFile(files[0])
			if err != nil || !strings.Contains(string(content), "invalid_tool_envelope") {
				t.Fatalf("missing original failure: %s err=%v", content, err)
			}
		})
	}
}
