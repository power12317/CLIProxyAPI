package handlers

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func TestHTTPBusinessBodyArrivesBeforeDiagnosticFinalization(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream"} {
		t.Run(contentType, func(t *testing.T) {
			body := `{"result":"native completed"}`
			if contentType == "text/event-stream" {
				body = "data: {\"type\":\"response.completed\"}\n\n"
			}
			entered, release, finished := make(chan struct{}), make(chan struct{}), make(chan struct{})
			var once sync.Once
			unblock := func() { once.Do(func() { close(release) }) }
			handler := &BaseAPIHandler{Cfg: &config.SDKConfig{}}
			router := gin.New()
			router.GET("/response", func(c *gin.Context) {
				ctx, cancel := handler.GetContextWithCancel(nil, c, context.Background())
				logging.DeferDiagnostics(ctx, func() {
					close(entered)
					<-release
					// Gin must still belong to this request while trailing logs finish.
					c.Set("diagnostics-finished", true)
				})
				c.Data(http.StatusOK, contentType, []byte(body))
				cancel()
				if got, _ := c.Get("diagnostics-finished"); got != true {
					t.Error("request ended before diagnostic cleanup")
				}
				close(finished)
			})
			server := httptest.NewServer(router)
			defer server.Close()
			defer unblock()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, http.MethodGet, server.URL+"/response", nil)
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal("HTTP headers waited for diagnostics", err)
			}
			defer func() { _ = response.Body.Close() }()
			got := make([]byte, len(body))
			if _, err = io.ReadFull(response.Body, got); err != nil || string(got) != body {
				t.Fatalf("business output waited for diagnostics: %q, %v", got, err)
			}
			<-entered
			select {
			case <-finished:
				t.Fatal("diagnostics were not blocked")
			default:
			}
			unblock()
			<-finished
		})
	}
}
