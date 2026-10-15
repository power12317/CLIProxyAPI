package helps

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestCodexSourceBundleOriginalHourAndSnapshot(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	auth := ticketProbeTestAuth(t.Name(), "source@example.com", "source-account")
	state := borrowTestState(now, 1)
	resp := &http.Response{Header: make(http.Header)}
	resp.Header.Add("Set-Cookie", "__cflb=cf-original; Max-Age=3600; Secure; Path=/")
	resp.Header.Add("Set-Cookie", "__oailb=oai-original; Max-Age=3600; Secure; Path=/")
	ticket := newCodexSourceTicket(auth, "gpt-6-astra", state, resp, now)
	StoreCodexTurnStateTicket(auth, ticket)
	again := newCodexSourceTicket(auth, "gpt-6-astra", state, resp, now.Add(50*time.Minute))
	if !again.CapturedAt.Equal(now) || !again.ExpiresAt.Equal(now.Add(time.Hour)) || !again.Route.CFLB.ExpiresAt.Equal(now.Add(time.Hour)) {
		t.Fatal("same ticket extended original deadlines")
	}
	StoreCodexTurnStateTicket(auth, again)
	// Reload credential metadata and let an unrelated response mutate the jar.
	// Neither operation changes the independent process-local ticket snapshot.
	raw, _ := json.Marshal(auth.Metadata)
	if err := json.Unmarshal(raw, &auth.Metadata); err != nil {
		t.Fatal(err)
	}
	target, _ := url.Parse(codexTurnStateTicketProbeURL)
	jar := CodexCookieJarForAuth(auth)
	t.Cleanup(func() { InvalidateCodexCookieJar(auth.ID) })
	jar.SetCookies(target, []*http.Cookie{{Name: "__cflb", Value: "unrelated"}, {Name: "__oailb", Value: "unrelated"}})
	for range 3 {
		exported, err := ExportCodexSourceTicket(auth, "gpt-6-astra", now.Add(50*time.Minute))
		if err != nil || exported.State != state || exported.Route.CFLB.Value != "cf-original" || exported.Route.OaiLB.Value != "oai-original" {
			t.Fatal("50-minute source no longer exportable, or jar mixed the snapshot")
		}
	}
	if _, err := ExportCodexSourceTicket(auth, "gpt-6-astra", now.Add(time.Hour)); err == nil {
		t.Fatal("source survived its original one-hour limit")
	}
	copied := auth.Clone()
	copied.Metadata["codex_client_system"] = "windows"
	if _, err := ExportCodexSourceTicket(copied, "gpt-6-astra", now); err == nil {
		t.Fatal("another owner reused the memory snapshot")
	}
}

func TestCodexSourceBundleMissingDeletedAndExpiredCookies(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	auth := ticketProbeTestAuth(t.Name(), "source@example.com", "source-account")
	for _, headers := range [][]string{
		nil, {"__cflb=cf; Path=/"}, {"__cflb=cf; Path=/", "__oailb=; Max-Age=0; Path=/"},
		{"__cflb=cf; Path=/", "__oailb=x; Expires=Wed, 01 Jan 2020 00:00:00 GMT; Path=/"},
	} {
		auth.Metadata = map[string]any{"access_token": "source-token", "account_id": "source-account"}
		resp := &http.Response{Header: make(http.Header)}
		for _, v := range headers {
			resp.Header.Add("Set-Cookie", v)
		}
		ticket := newCodexSourceTicket(auth, "model", borrowTestState(now, 1), resp, now)
		StoreCodexTurnStateTicket(auth, ticket)
		if _, err := ExportCodexSourceTicket(auth, "model", now); err == nil {
			t.Fatal("incomplete source exported")
		}
	}
	route := borrowTestRoute()
	route.CFLB.ExpiresAt = now.Add(3 * time.Minute)
	ticket := CodexTurnStateTicket{Model: "model", Owner: CodexOwnerFingerprint(auth), State: borrowTestState(now, 2), CapturedAt: now, ExpiresAt: now.Add(time.Hour), Route: &route}
	StoreCodexTurnStateTicket(auth, ticket)
	if _, err := ExportCodexSourceTicket(auth, "model", now.Add(3*time.Minute)); err == nil {
		t.Fatal("expired source cookie exported")
	}
}

func TestCodexSourceCapturesActualExchangeBeforeJarChanges(t *testing.T) {
	auth := ticketProbeTestAuth(t.Name(), "source@example.com", "source-account")
	t.Cleanup(func() { InvalidateCodexCookieJar(auth.ID) })
	target, _ := url.Parse(codexTurnStateTicketProbeURL)
	jar := CodexCookieJarForAuth(auth)
	jar.SetCookies(target, []*http.Cookie{{Name: "__cflb", Value: "cf-sent", MaxAge: 120}, {Name: "__oailb", Value: "oai-sent", MaxAge: 3600}})
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", codexRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		jar.SetCookies(target, []*http.Cookie{{Name: "__cflb", Value: "unrelated"}, {Name: "__oailb", Value: "unrelated"}})
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}))
	client := NewUtlsHTTPClient(ctx, &config.Config{}, auth, 0)
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, target.String(), nil)
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	route, ok := sourceResponseRoute(resp, time.Now())
	if !ok || route.CFLB.Value != "cf-sent" || route.OaiLB.Value != "oai-sent" || route.CFLB.ExpiresAt.IsZero() || time.Until(route.CFLB.ExpiresAt) > 120*time.Second {
		t.Fatalf("did not snapshot original exchange: %#v", route)
	}
}

func TestCodexSourceKeepsCompleteSnapshotWhenNewResponseLacksPair(t *testing.T) {
	now := time.Now().Truncate(time.Second)
	auth := ticketProbeTestAuth(t.Name(), "source@example.com", "source")
	route := borrowTestRoute()
	old := CodexTurnStateTicket{Model: "model", Owner: CodexOwnerFingerprint(auth), State: borrowTestState(now, 1), CapturedAt: now, ExpiresAt: now.Add(time.Hour), Route: &route}
	StoreCodexTurnStateTicket(auth, old)
	next := newCodexSourceTicket(auth, "model", borrowTestState(now.Add(time.Minute), 2), &http.Response{Header: make(http.Header)}, now.Add(time.Minute))
	if next.State != old.State || next.Route == nil {
		t.Fatal("incomplete replacement destroyed available source")
	}
}

func TestCodexTicketProbePayloadRulesAreFinal(t *testing.T) {
	cfg, err := config.ParseConfigBytes([]byte(`payload:
  override:
    - models:
        - name: gpt-6-astra
          protocol: codex
      params:
        instructions: custom-final-instructions
  filter:
    - models:
        - name: gpt-6-astra
          protocol: codex
      params:
        - client_metadata
`))
	if err != nil {
		t.Fatal(err)
	}
	auth := ticketProbeTestAuth(t.Name(), "x@example.com", "account")
	req, _, _, err := newCodexTicketProbeRequest(t.Context(), cfg, auth, "gpt-6-astra")
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(req.Body)
	if strings.Contains(string(raw), "client_metadata") || !strings.Contains(string(raw), "custom-final-instructions") {
		t.Fatalf("payload rules were overwritten: %s", raw)
	}
}
