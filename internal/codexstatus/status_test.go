package codexstatus

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestDetectionRuleAndFixedTurnWindow(t *testing.T) {
	for _, tc := range []struct {
		name, turn        string
		request, response int
		age               time.Duration
		want              string
	}{
		{"match", "turn", 780, 780, 0, "degraded"},
		{"empty request", "turn", 0, 780, 0, "normal"},
		{"empty response", "turn", 780, 0, 0, "normal"},
		{"both empty", "turn", 0, 0, 0, "normal"},
		{"different request", "turn", 779, 780, 0, "normal"},
		{"different response", "turn", 780, 779, 0, "normal"},
		{"missing turn", "", 780, 780, 0, "normal"},
		{"one hour", "turn", 780, 780, time.Hour, "degraded"},
		{"expired", "turn", 780, 780, time.Hour + time.Nanosecond, "normal"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := New(config.EventNotifications{})
			defer m.Close()
			now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
			m.now = func() time.Time { return now }
			m.Request().Begin(Credential{ID: "first"}, tc.turn, 0)
			now = now.Add(tc.age)
			r := m.Request()
			r.Begin(Credential{ID: "retry"}, tc.turn, tc.request).Observe(tc.response)
			if got := r.Status(); got != tc.want {
				t.Fatalf("status=%s, want %s", got, tc.want)
			}
		})
	}
}

func TestRequestsAreIndependentOfCredentialHistoryAndNotificationCooldown(t *testing.T) {
	m := New(config.EventNotifications{InstanceName: "Tokyo", Platform: "feishu", Webhook: "https://example.invalid"})
	defer m.Close()
	deliveries := make(chan string, 8)
	m.send = func(_ context.Context, _ config.EventNotifications, message string) error {
		deliveries <- message
		return nil
	}
	start := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	now := start
	m.now = func() time.Time { return now }
	c := Credential{ID: "account"}
	check := func(turn string, request, response int, want string) {
		t.Helper()
		r := m.Request()
		r.Begin(c, turn, request).Observe(response)
		if got := r.Status(); got != want {
			t.Fatalf("%d/%d status=%s, want %s", request, response, got, want)
		}
	}
	check("turn", 780, 780, "degraded")
	awaitNotification(t, deliveries)
	now = start.Add(30 * time.Minute)
	check("turn", 0, 780, "normal")
	check("turn", 780, 0, "normal")
	check("turn", 0, 0, "normal")
	check("turn", 780, 780, "degraded")
	if m.notified[credentialKey{c.ID, c.Owner}] != start {
		t.Fatal("a request inside cooldown changed the notification time")
	}
	now = start.Add(time.Hour + time.Nanosecond)
	check("turn", 780, 780, "normal")
	if m.notified[credentialKey{c.ID, c.Owner}] != start {
		t.Fatal("an expired turn triggered a notification")
	}
	check("new-turn", 780, 780, "degraded")
	awaitNotification(t, deliveries)
	if m.notified[credentialKey{c.ID, c.Owner}] != now {
		t.Fatal("a new turn did not notify after cooldown")
	}
}

func TestWebsocketRequestsHaveIndependentStatuses(t *testing.T) {
	m := New(config.EventNotifications{})
	defer m.Close()
	r := m.Request()
	r.negotiated = true
	c := Credential{ID: "account"}
	var version uint64
	for _, tc := range []struct {
		request, response int
		want              string
	}{
		{780, 780, "degraded"},
		{0, 780, "normal"},
		{780, 0, "normal"},
		{780, 780, "degraded"},
	} {
		r.Reset()
		r.Begin(c, "turn", tc.request).Observe(tc.response)
		next, frame := r.WebsocketStatus(version)
		if got, want := string(frame), `{"type":"cpa.status","status":"`+tc.want+`"}`; got != want {
			t.Fatalf("frame=%s, want %s", got, want)
		}
		version = next
	}
	old := r.Begin(c, "turn", 780)
	r.Reset()
	r.Begin(c, "turn", 0).Observe(780)
	old.Observe(780)
	if r.Status() != "normal" {
		t.Fatal("late metadata from a previous request changed the new request")
	}
}

func awaitNotification(t *testing.T, deliveries <-chan string) string {
	t.Helper()
	select {
	case message := <-deliveries:
		return message
	case <-time.After(time.Second):
		t.Fatal("notification missing")
		return ""
	}
}

func TestNotificationsAreDirectAndCooldownIsPerCredentialInMemory(t *testing.T) {
	cfg := config.EventNotifications{InstanceName: "Tokyo", Platform: "feishu", Webhook: "https://example.invalid"}
	deliveries := make(chan string, 32)
	send := func(_ context.Context, _ config.EventNotifications, message string) error {
		deliveries <- message
		return nil
	}
	m := New(cfg)
	defer m.Close()
	m.send = send
	now := time.Now()
	m.now = func() time.Time { return now }
	c := Credential{ID: "a", Email: "user@example.com", File: "user.json"}
	m.notify(c)
	if got := awaitNotification(t, deliveries); got != "CPA 实例「Tokyo」的账号「user@example.com」凭据「user.json」可能出现降智。" {
		t.Fatal(got)
	}
	m.notify(c)
	for i := 0; i < 20; i++ {
		m.notify(Credential{ID: fmt.Sprint(i)})
	}
	for i := 0; i < 20; i++ {
		if got := awaitNotification(t, deliveries); strings.Contains(got, "user@example.com") {
			t.Fatal("cooldown failed")
		}
	}
	now = now.Add(time.Hour)
	m.notify(c)
	awaitNotification(t, deliveries)
	restarted := New(cfg)
	defer restarted.Close()
	restarted.send = send
	restarted.notify(c)
	awaitNotification(t, deliveries)
	restarted.Update(config.EventNotifications{})
	restarted.notify(Credential{ID: "disabled"})
	if len(restarted.notified) != 1 {
		t.Fatal("empty webhook must disable notification")
	}
}

func TestStatusTransportPreservesDegradationAndBusinessBytes(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, negotiate := range []bool{false, true} {
			t.Run(fmt.Sprintf("stream=%v/opt-in=%v", stream, negotiate), func(t *testing.T) {
				m := New(config.EventNotifications{})
				defer m.Close()
				router := gin.New()
				router.Use(m.Middleware())
				router.GET("/", func(c *gin.Context) {
					if c.GetHeader(Header) != "" {
						t.Fatal("status opt-in leaked upstream")
					}
					r := FromContext(c.Request.Context())
					a := r.Begin(Credential{ID: "a"}, "turn", 780)
					if stream {
						c.Header("Content-Type", "text/event-stream")
						_, _ = c.Writer.WriteString("data: first")
						a.Observe(780)
						_, _ = c.Writer.WriteString("\n\n")
						r.Begin(Credential{ID: "b"}, "turn", 779).Observe(779)
						_, _ = c.Writer.WriteString("data: [DONE]\n\n")
					} else {
						c.Header("Content-Type", "application/json")
						_, _ = c.Writer.WriteString("\n")
						c.Writer.Flush()
						a.Observe(780)
						r.Begin(Credential{ID: "b"}, "turn", 779).Observe(779)
						_, _ = c.Writer.WriteString(`{"output":[]}`)
					}
				})
				server := httptest.NewServer(router)
				defer server.Close()
				req, _ := http.NewRequest(http.MethodGet, server.URL, nil)
				if negotiate {
					req.Header.Set(Header, "1")
				}
				resp, err := server.Client().Do(req)
				if err != nil {
					t.Fatal(err)
				}
				body, err := io.ReadAll(resp.Body)
				if err != nil {
					t.Fatal(err)
				}
				if err = resp.Body.Close(); err != nil {
					t.Fatal(err)
				}
				want := "\n" + `{"output":[]}`
				if stream {
					want = "data: first\n\n"
					if negotiate {
						want += StreamPrefix + "degraded\n\n"
					}
					want += "data: [DONE]\n\n"
				}
				if string(body) != want {
					t.Fatalf("body=%q, want %q", body, want)
				}
				if negotiate && !stream && resp.Trailer.Get(Header) != "degraded" {
					t.Fatal("late degradation missing")
				}
				if !negotiate && (resp.Header.Get(Header) != "" || resp.Trailer.Get(Header) != "") {
					t.Fatal("status exposed without opt-in")
				}
			})
		}
	}
}
