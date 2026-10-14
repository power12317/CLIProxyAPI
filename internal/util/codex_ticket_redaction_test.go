package util

import "testing"

func TestCodexTicketAndCookieHeadersAreRedacted(t *testing.T) {
	for _, key := range []string{"Cookie", "Set-Cookie", "set-cookie2", "X-Codex-Turn-State", "x-codex-turn-state"} {
		if got := MaskSensitiveHeaderValue(key, "private-ticket-or-cookie"); got != "[REDACTED]" {
			t.Errorf("%s was not redacted", key)
		}
	}
	if got := MaskSensitiveHeaderValue("Content-Type", "text/event-stream"); got != "text/event-stream" {
		t.Fatal("ordinary header was changed")
	}
}
