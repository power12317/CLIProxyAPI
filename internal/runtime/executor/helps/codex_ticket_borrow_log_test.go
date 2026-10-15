package helps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func captureBorrowLogs(t *testing.T) *logtest.Hook {
	t.Helper()
	logger := log.StandardLogger()
	hooks, level := logger.ReplaceHooks(make(log.LevelHooks)), logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	t.Cleanup(func() {
		logger.ReplaceHooks(hooks)
		logger.SetLevel(level)
	})
	return hook
}

func formattedBorrowLogs(t *testing.T, hook *logtest.Hook) string {
	t.Helper()
	var out strings.Builder
	for _, entry := range hook.AllEntries() {
		if entry.Data[logging.CodexTicketBorrowLogField] != true {
			continue
		}
		line, err := (&logging.LogFormatter{}).Format(entry)
		if err != nil {
			t.Fatal(err)
		}
		out.Write(line)
	}
	return out.String()
}

func TestCodexBorrowLogsCorrelateCompleteFlowAndRevocation(t *testing.T) {
	hook := captureBorrowLogs(t)
	f := newBorrowFixture(t)
	f.auth.FileName = "codex-b-windows.json"
	f.seed.AccountID = "private-donor@example.invalid-windows.json"
	f.cfg.CodexHeaderDefaults.OaiLBBorrow.SourceAuthID = f.seed.AccountID
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	logging.SetGinRequestID(c, "a1b2c3d4")
	logging.SetCodexTurnStateLogFields(c, logging.CodexTurnStateLogFields{SessionID: "session-business", TurnID: "turn-business"})
	f.ctx = context.WithValue(f.ctx, "gin", c)
	ticket := f.acquire(t)
	entries := hook.AllEntries()
	stages := []string{"ACQUIRE", "SOURCE", "EXCHANGE", "VERIFY", "READY"}
	if len(entries) != len(stages) {
		t.Fatalf("unexpected log count: %d", len(entries))
	}
	for i, stage := range stages {
		if !strings.HasPrefix(entries[i].Message, stage) || entries[i].Data["request_id"] != "a1b2c3d4" || entries[i].Data["auth_file"] != "codex-b-windows.json" {
			t.Fatalf("uncorrelated stage %s: %+v", stage, entries[i])
		}
	}
	for _, want := range []string{"SOURCE 200", "0/780", "EXCHANGE 200", "780/780", "ticket=changed", "VERIFY 200", "780/0", "ticket=absent", "READY | gpt-6-astra | ticket=780"} {
		if !strings.Contains(formattedBorrowLogs(t, hook), want) {
			t.Errorf("missing %q", want)
		}
	}
	f.ctx = logging.WithRequestID(f.ctx, "b1b2c3d4")
	if f.acquire(t) != ticket || f.fetches.Load() != 1 || f.probes.Load() != 2 {
		t.Fatal("logging altered cache reuse")
	}
	req, _ := http.NewRequestWithContext(f.ctx, http.MethodPost, codexTurnStateTicketProbeURL, nil)
	resp := &http.Response{Request: req, Header: http.Header{CodexTurnStateTicketHeader: {borrowTestState(time.Now(), 99)}}}
	ticket.ObserveResponse(resp)
	ticket.ObserveResponse(resp)
	lines := formattedBorrowLogs(t, hook)
	if !strings.Contains(lines, "[b1b2c3d4] [codex-b-windows.json] [session] [turn] [info ] [TICKET-BORROW] REUSE") || strings.Count(lines, "[TICKET-BORROW] REVOKE") != 1 {
		t.Fatalf("missing reuse/revocation correlation: %s", lines)
	}
	for _, secret := range []string{f.cfg.CodexHeaderDefaults.OaiLBBorrow.SourceAuthID, "credential=", "__cflb=", "__oailb=", f.seed.State, ticket.State, "source-cf", "source-oai", "management-secret", f.auth.Metadata["access_token"].(string), "[TICKET-PROBE]", "result=", ".go:"} {
		if strings.Contains(lines, secret) {
			t.Errorf("log disclosed %q", secret)
		}
	}
	for _, entry := range hook.AllEntries() {
		if strings.Contains(entry.Message+fmt.Sprint(entry.Data), f.cfg.CodexHeaderDefaults.OaiLBBorrow.SourceAuthID) {
			t.Fatal("source credential identifier leaked through a structured log field")
		}
	}
}

func TestCodexBorrowStreamFailureIncludesFlatErrorCodeWithoutPrivateMessage(t *testing.T) {
	for _, raw := range []string{
		`{"type":"error","code":"rate_limit_exceeded","message":"private-source.json private-ticket"}`,
		`{"type":"error","error":{"code":"rate_limit_exceeded","message":"private-source.json private-ticket"}}`,
	} {
		err := readCodexBorrowComplete(strings.NewReader("data: "+raw+"\n\n"), "private-ticket")
		if err == nil || !strings.Contains(err.Error(), "code=rate_limit_exceeded") || strings.Contains(err.Error(), "private-") {
			t.Fatalf("missing safe upstream error code: %v", err)
		}
	}
	err := readCodexBorrowComplete(strings.NewReader("data: "+`{"type":"error","code":"private-source.json","error":{"type":"private-ticket"},"message":"private-token"}`+"\n\n"), "private-ticket", "private-token")
	if err == nil || strings.Contains(err.Error(), "private-") {
		t.Fatalf("unrecognized error fields must not leak raw values: %v", err)
	}
}

func TestCodexBorrowLogsRetainSameLengthFailureDuringThrottle(t *testing.T) {
	hook := captureBorrowLogs(t)
	f := newBorrowFixture(t)
	f.ctx = logging.WithRequestID(f.ctx, "1234abcd")
	f.shot = func(req *http.Request, n int32) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Header: http.Header{CodexTurnStateTicketHeader: {borrowTestState(time.Now(), byte(n+1))}}, Body: io.NopCloser(strings.NewReader(borrowTestComplete)), Request: req}, nil
	}
	_, err := AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-6-astra")
	want := "verify: continuation returned a different ticket (780/780)"
	if err == nil || !strings.Contains(err.Error(), want) {
		t.Fatalf("missing actual verification failure: %v", err)
	}
	f.ctx = logging.WithRequestID(f.ctx, "5678abcd")
	_, err = AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-6-astra")
	if err == nil || !strings.Contains(err.Error(), want) || !strings.Contains(err.Error(), "acquisition request: 1234abcd") || f.fetches.Load() != 1 || f.probes.Load() != 2 {
		t.Fatalf("throttle lost failure or performed network requests: %v", err)
	}
	var failure *CodexTicketBorrowError
	if !errors.As(err, &failure) || failure.StatusCode() != 503 || !failure.IsRequestScoped() || *failure.RetryAfter() != 30*time.Second {
		t.Fatalf("request-scoped retry policy changed: %v", err)
	}
	lines := formattedBorrowLogs(t, hook)
	for _, wantLog := range []string{"VERIFY 200", "780/780", "ticket=changed", want, "WAIT | gpt-6-astra", "business request not sent"} {
		if !strings.Contains(lines, wantLog) {
			t.Errorf("missing %q", wantLog)
		}
	}
	// Advance the retry boundary explicitly, then ensure success clears the old reason.
	codexBorrowedTickets.Lock()
	for _, entry := range codexBorrowedTickets.entries {
		if entry.lastRequestID == "1234abcd" {
			entry.retryAt = time.Now().Add(-time.Second)
		}
	}
	codexBorrowedTickets.Unlock()
	f.shot = nil
	ticket := f.acquire(t)
	codexBorrowedTickets.Lock()
	lastFailure := codexBorrowedTickets.entries[ticket.key].lastFailure
	codexBorrowedTickets.Unlock()
	if lastFailure != "" {
		t.Fatalf("successful renewal retained stale error: %s", lastFailure)
	}
	ticket.ExpiresAt = time.Now().Add(50 * time.Second)
	f.failSource.Store(true)
	if f.acquire(t) != ticket || !strings.Contains(formattedBorrowLogs(t, hook), "retaining unexpired verified ticket") {
		t.Fatal("failed renewal did not log retained ticket")
	}
}

func TestCodexBorrowRetryDiagnosticUsesRemainingInterval(t *testing.T) {
	now := time.Date(2026, 10, 15, 1, 0, 0, 0, time.UTC)
	retry := now.Add(30 * time.Second)
	for _, tc := range []struct {
		elapsed time.Duration
		seconds int
	}{{0, 30}, {time.Millisecond, 30}, {17 * time.Second, 13}, {29999 * time.Millisecond, 1}} {
		got := codexBorrowRetryReason(retry, now.Add(tc.elapsed), "verify: example", "a1b2c3d4")
		if !strings.Contains(got, fmt.Sprintf("deferred for %d seconds", tc.seconds)) || !strings.Contains(got, "verify: example") {
			t.Fatalf("incorrect retry diagnostic: %s", got)
		}
	}
}

type borrowDelayedErrorReader struct{}

func (borrowDelayedErrorReader) Read([]byte) (int, error) {
	// This reader is used only inside synctest's virtual clock.
	time.Sleep(2 * time.Second)
	return 0, context.DeadlineExceeded
}

func TestCodexBorrowLogWaitsForBodyAndIncludesReadFailure(t *testing.T) {
	hook := captureBorrowLogs(t)
	f := newBorrowFixture(t)
	roundTripper := f.ctx.Value("cliproxy.roundtripper")
	synctest.Test(t, func(t *testing.T) {
		f.ctx = logging.WithRequestID(context.WithValue(t.Context(), "cliproxy.roundtripper", roundTripper), "deadbeef")
		f.shot = func(req *http.Request, _ int32) (*http.Response, error) {
			return &http.Response{StatusCode: 200, Header: http.Header{CodexTurnStateTicketHeader: {borrowTestState(time.Now(), 2)}}, Body: io.NopCloser(borrowDelayedErrorReader{}), Request: req}, nil
		}
		_, _, err := probeCodexBorrow(f.ctx, f.cfg, f.auth, "gpt-6-astra", f.seed.State, *f.seed.Route, newCodexBorrowLog(f.ctx, f.auth, "gpt-6-astra"), "EXCHANGE")
		if err == nil || !strings.Contains(err.Error(), "probe stream read failed: deadline exceeded") {
			t.Fatalf("lost read failure: %v", err)
		}
		entries := hook.AllEntries()
		if len(entries) != 1 || entries[0].Level != log.WarnLevel {
			t.Fatalf("200 headers incorrectly logged as successful completion: %+v", entries)
		}
		line := formattedBorrowLogs(t, hook)
		if !strings.Contains(line, "EXCHANGE 200 | 2.000s") || !strings.Contains(line, "780/780") || !strings.Contains(line, "deadline exceeded (0 bytes)") {
			t.Fatalf("missing full duration/response/failure: %s", line)
		}
	})
}

func TestCodexBorrowLogsSpecificFailuresWithoutSecrets(t *testing.T) {
	for _, tc := range []struct{ mode, reason string }{
		{"source", "source: source instance returned HTTP 503"},
		{"echo", "exchange: response repeated the source ticket"},
		{"structure", "exchange: invalid ticket structure"},
		{"incomplete", "exchange: probe stream did not complete"},
		{"sse", "exchange: probe stream reported failure: type=response.failed, response.status=failed, response.error.code=rate_limit_exceeded"},
		{"partial-cookie", "exchange: probe response updated only one routing cookie"},
		{"changed-cookie", "verify: continuation changed cookie values (__cflb=true, __oailb=true)"},
		{"transport", "exchange: probe transport failed: I/O failure (*url.Error)"},
	} {
		t.Run(tc.mode, func(t *testing.T) {
			hook := captureBorrowLogs(t)
			f := newBorrowFixture(t)
			f.failSource.Store(tc.mode == "source")
			f.shot = func(req *http.Request, n int32) (*http.Response, error) {
				h := http.Header{}
				if n == 1 {
					h.Set(CodexTurnStateTicketHeader, borrowTestState(time.Now(), 2))
				}
				body := borrowTestComplete
				switch tc.mode {
				case "echo":
					h.Set(CodexTurnStateTicketHeader, f.seed.State)
				case "structure":
					h.Set(CodexTurnStateTicketHeader, strings.Repeat("x", 780))
				case "incomplete":
					body = "data: [DONE]\n\n"
				case "sse":
					body = `data: {"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"request rejected for source-auth"}}}` + "\n\n"
				case "partial-cookie":
					h.Add("Set-Cookie", "__cflb=secret-cookie-value; Path=/")
				case "changed-cookie":
					if n == 2 {
						h.Add("Set-Cookie", "__cflb=secret-cookie-value; Path=/")
						h.Add("Set-Cookie", "__oailb=secret-cookie-value; Path=/")
					}
				case "transport":
					return nil, fmt.Errorf("secret-transport-value")
				}
				return &http.Response{StatusCode: 200, Header: h, Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}
			_, err := AcquireCodexBorrowedTicket(f.ctx, f.cfg, f.auth, "gpt-6-astra")
			if err == nil || !strings.Contains(err.Error(), tc.reason) {
				t.Fatalf("failure is not specific: %v, want %s", err, tc.reason)
			}
			lines := formattedBorrowLogs(t, hook)
			if !strings.Contains(lines, tc.reason) {
				t.Fatalf("failure missing in log: %s", lines)
			}
			for _, secret := range []string{f.cfg.CodexHeaderDefaults.OaiLBBorrow.SourceAuthID, "credential=", "secret-body-value", "secret-cookie-value", "secret-transport-value", f.seed.State, "management-secret"} {
				if strings.Contains(lines, secret) || strings.Contains(err.Error(), secret) {
					t.Fatalf("secret leaked: %s", secret)
				}
			}
		})
	}
	u, _ := url.Parse("https://user:secret@source.example:8080/prefix?key=secret#secret")
	if got := codexBorrowLogURL(u); got != "https://source.example:8080/prefix" {
		t.Fatalf("unsafe source URL: %s", got)
	}
}
