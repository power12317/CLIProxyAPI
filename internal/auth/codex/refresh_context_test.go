package codex

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
)

func TestRefreshModeStopsRequestsAndRetriesWithCredentialLog(t *testing.T) {
	resetCodexRefreshGroupForTest()
	t.Cleanup(resetCodexRefreshGroupForTest)
	var output bytes.Buffer
	logger := log.StandardLogger()
	oldOutput, oldFormatter := logger.Out, logger.Formatter
	logger.SetOutput(&output)
	logger.SetFormatter(&logging.LogFormatter{})
	t.Cleanup(func() { logger.SetOutput(oldOutput); logger.SetFormatter(oldFormatter) })
	allowed := false
	calls := 0
	service := &CodexAuth{httpClient: &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		calls++
		allowed = false
		return &http.Response{StatusCode: 401, Body: io.NopCloser(strings.NewReader(`{"error":{"code":"refresh_token_invalidated"}}`)), Header: make(http.Header)}, nil
	})}}
	ctx := WithRefreshCredential(context.Background(), "account-original.json", func() bool { return allowed })
	if _, err := service.RefreshTokensWithRetry(ctx, "secret-refresh-value", 3); err == nil || calls != 0 {
		t.Fatal("Codex mode sent a refresh request")
	}
	allowed = true
	if _, err := service.RefreshTokensWithRetry(ctx, "secret-refresh-value", 3); err == nil || calls != 1 {
		t.Fatal("mode switch did not stop subsequent retries")
	}
	for _, want := range []string{"account-original.json", "refresh_owner=cpa", "status 401", "refresh_token_invalidated"} {
		if !strings.Contains(output.String(), want) {
			t.Fatalf("rendered refresh log does not contain %q: %s", want, output.String())
		}
	}
	if strings.Contains(output.String(), "secret-refresh-value") {
		t.Fatal("refresh token was logged")
	}
}
