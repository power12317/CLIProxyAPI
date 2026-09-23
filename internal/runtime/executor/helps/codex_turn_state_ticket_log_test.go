package helps

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
	"github.com/tidwall/gjson"
)

func TestHarvestCodexTurnStateTicketLogsRawResponseLengths(t *testing.T) {
	logger := log.StandardLogger()
	previousHooks := logger.ReplaceHooks(make(log.LevelHooks))
	previousLevel := logger.GetLevel()
	previousCaller := logger.ReportCaller
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.InfoLevel)
	logger.SetReportCaller(true)
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetLevel(previousLevel)
		logger.SetReportCaller(previousCaller)
	})

	for _, tc := range []struct {
		name         string
		plan         string
		status       int
		length       int
		transportErr error
		wantTicket   bool
		wantLevel    log.Level
	}{
		{name: "personal 780", status: 200, length: 780, wantTicket: true, wantLevel: log.InfoLevel},
		{name: "personal 312 remains visible", status: 200, length: 312, wantLevel: log.InfoLevel},
		{name: "team 780", plan: "team", status: 200, length: 780, wantTicket: true, wantLevel: log.InfoLevel},
		{name: "no response header", status: 200, wantLevel: log.InfoLevel},
		{name: "429 still records 312", status: 429, length: 312, wantLevel: log.WarnLevel},
		{name: "upstream 500", status: 500, wantLevel: log.ErrorLevel},
		{name: "timeout", transportErr: context.DeadlineExceeded, wantLevel: log.WarnLevel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hook.Reset()
			auth := &cliproxyauth.Auth{
				ID: "probe-test/" + tc.name, FileName: "codex-user-team-windows.json", Provider: "codex",
				Attributes: map[string]string{cliproxyauth.AttributeAuthKind: cliproxyauth.AuthKindOAuth, "plan_type": tc.plan},
				Metadata:   map[string]any{"access_token": "secret-access-token"},
			}
			t.Cleanup(func() { InvalidateCodexCookieJar(auth.ID) })
			var sessionID, turnID string
			state := ""
			if tc.length > 0 {
				state = testTicketState(tc.length)
			}
			rt := codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				sessionID = req.Header.Get("Session-Id")
				body, errRead := io.ReadAll(req.Body)
				if errRead != nil {
					return nil, errRead
				}
				turnID = gjson.Get(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String(), "turn_id").String()
				if tc.transportErr != nil {
					return nil, tc.transportErr
				}
				return &http.Response{StatusCode: tc.status, Header: http.Header{CodexTurnStateTicketHeader: {state}}, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
			})
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(rt))
			ticket, status, errHarvest := HarvestCodexTurnStateTicket(ctx, &config.Config{}, auth, "gpt-6-astra", "")
			if (errHarvest != nil) != (tc.transportErr != nil) || status != tc.status {
				t.Fatalf("status=%d err=%v", status, errHarvest)
			}
			if (ticket.State != "") != tc.wantTicket {
				t.Fatalf("ticket accepted=%v, want %v", ticket.State != "", tc.wantTicket)
			}
			entries := hook.AllEntries()
			if len(entries) != 1 {
				t.Fatalf("log count=%d, want one per probe", len(entries))
			}
			entry := entries[0]
			if entry.Level != tc.wantLevel {
				t.Fatalf("level=%v, want %v", entry.Level, tc.wantLevel)
			}
			formatted, errFormat := (&logging.LogFormatter{}).Format(entry)
			if errFormat != nil {
				t.Fatal(errFormat)
			}
			statusText := "---"
			if tc.status > 0 {
				statusText = fmt.Sprint(tc.status)
			}
			pattern := fmt.Sprintf(`^\[\d{4}-\d{2}-\d{2} \d{2}:\d{2}:\d{2}\] \[[0-9a-f]{8}\] \[codex-user-team-windows.json\] \[%s\] \[%s\] \[(info |warn |error)\] \[TICKET-PROBE\] %s \| \d+\.\d{3}s \| gpt-6-astra/- \| 0/%d \| POST "/backend-api/codex/responses"\n$`,
				regexp.QuoteMeta(logging.ShortCodexIdentifier(sessionID)), regexp.QuoteMeta(logging.ShortCodexIdentifier(turnID)), statusText, tc.length)
			if !regexp.MustCompile(pattern).Match(formatted) {
				t.Fatalf("unexpected log format: %s", formatted)
			}
			for _, forbidden := range []string{"secret-access-token", "gAAAAA", "target=", "result=", "internal", ".go:"} {
				if strings.Contains(string(formatted), forbidden) {
					t.Fatalf("log contains forbidden value %q", forbidden)
				}
			}
		})
	}
}
