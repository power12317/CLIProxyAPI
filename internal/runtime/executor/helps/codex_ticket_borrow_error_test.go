package helps

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestCodexBorrowUpstreamErrorsReachLogsAndRetryResponse(t *testing.T) {
	for _, mode := range []string{"exchange-sse", "verify-sse", "exchange-http", "source-http"} {
		t.Run(mode, func(t *testing.T) {
			hook := captureBorrowLogs(t)
			f := newBorrowFixture(t)
			f.cfg.CodexHeaderDefaults.OaiLBBorrow.SourceAuthFile = "private-donor@example.invalid.json"
			message := "Too many concurrent requests. " + f.cfg.CodexHeaderDefaults.OaiLBBorrow.SourceAuthFile + " source-auth source-cf source-oai " + f.auth.Metadata["access_token"].(string) + "\nPlease wait."
			fields := map[string]any{"type": "error", "code": "concurrent_request_limit_exceeded", "message": message, "status": 429}
			raw, err := json.Marshal(fields)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "source-http" {
				// Source errors must not reveal the management key or selected source.
				body, _ := json.Marshal(map[string]any{"error": map[string]string{"code": "source_temporarily_unavailable", "message": "Try later: source-auth management-secret private-donor@example.invalid.json"}})
				source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.WriteHeader(503)
					_, _ = w.Write(body)
				}))
				defer source.Close()
				f.cfg.CodexHeaderDefaults.OaiLBBorrow.SourceURL = source.URL
			} else {
				f.shot = func(req *http.Request, n int32) (*http.Response, error) {
					body, status := "data: "+string(raw)+"\n\n", 200
					if mode == "verify-sse" && n == 1 {
						body = borrowTestComplete
					}
					if mode == "exchange-http" {
						body, status = string(raw), 429
					}
					return &http.Response{StatusCode: status, Header: http.Header{CodexTurnStateTicketHeader: {borrowTestState(time.Now(), byte(n+1))}}, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
				}
			}
			_, err = AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-6-astra")
			if err == nil {
				t.Fatal("upstream error accepted")
			}
			first := err.Error()
			probes, fetches := f.probes.Load(), f.fetches.Load()
			_, err = AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-6-astra")
			if err == nil || f.probes.Load() != probes || f.fetches.Load() != fetches {
				t.Fatal("retry lost failure or sent extra requests")
			}
			logs := formattedBorrowLogs(t, hook)
			wantCode, wantMessage := "concurrent_request_limit_exceeded", "Too many concurrent requests."
			if mode == "source-http" {
				wantCode, wantMessage = "source_temporarily_unavailable", "Try later:"
			}
			for _, output := range []string{first, err.Error(), logs} {
				if !strings.Contains(output, wantCode) || !strings.Contains(output, wantMessage) {
					t.Fatalf("missing upstream details: %s", output)
				}
				for _, secret := range []string{"private-donor", "source-auth", "source-cf", "source-oai", "management-secret", f.auth.Metadata["access_token"].(string), "__cflb=", "__oailb="} {
					if strings.Contains(output, secret) {
						t.Fatalf("private material or cookie statistics leaked: %s", secret)
					}
				}
			}
			if strings.Contains(logs, "\nPlease wait.") {
				t.Fatal("upstream message injected a log line")
			}
			if mode == "verify-sse" && !strings.Contains(first, "verify: probe stream reported failure") {
				t.Fatal("verification phase was lost")
			}
		})
	}
}

func TestCodexBorrowErrorMessageRedactsBeforeTruncation(t *testing.T) {
	secret := strings.Repeat("private", 250)
	got := codexBorrowRedactErrorText("Details: "+secret+" Try again.", []string{secret})
	if got != "Details: [redacted] Try again." {
		t.Fatalf("secret truncated before redaction: %s", got)
	}
	got = codexBorrowRedactErrorText(strings.Repeat("retry later. ", 200), nil)
	if len([]rune(got)) != 1027 || !strings.HasSuffix(got, "...") {
		t.Fatal("unbounded error message")
	}
}
