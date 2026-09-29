package basispoints

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestStreamErrorPreservesUpstreamMetadata(t *testing.T) {
	for _, tc := range []struct {
		name, raw string
		status    int
		delay     time.Duration
		hasDelay  bool
	}{
		{"nested rate limit", `{"type":"response.failed","response":{"status":"failed","error":{"code":"rate_limit_exceeded","message":"TPM limit reached","headers":{"retry-after":"1","retry-after-ms":"15"}}}}`, 429, 15 * time.Millisecond, true},
		{"direct JSON response", `{"status":"failed","error":{"type":"rate_limit_error","headers":{"Retry-After":"1.5"}}}`, 429, 1500 * time.Millisecond, true},
		{"top-level rate limit", `{"type":"error","error":{"code":"slow_down","headers":{"RETRY-AFTER-MS":0}}}`, 429, 0, true},
		{"explicit status wins", `{"type":"error","status":503,"error":{"code":"rate_limit_exceeded","status":429,"headers":{"Retry-After":"1"}}}`, 503, 0, false},
		{"nested status code", `{"type":"response.failed","response":{"error":{"status_code":403,"message":"blocked"}}}`, 403, 0, false},
		{"nested status", `{"type":"response.failed","response":{"error":{"status":422,"message":"invalid input"}}}`, 422, 0, false},
		{"unknown failure", `{"type":"response.failed","response":{"error":{"code":"unknown","message":"failed"}}}`, 502, 0, false},
		{"invalid explicit status", `{"status":200,"error":{"code":"rate_limit_exceeded"}}`, 429, 0, false},
		{"invalid JSON", `{`, 502, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := StreamError([]byte(tc.raw))
			var upstream *Error
			if !errors.As(err, &upstream) || upstream.StatusCode() != tc.status || err.Error() != tc.raw {
				t.Fatalf("status or original error changed: %#v", err)
			}
			delay := upstream.RetryAfter()
			if (delay != nil) != tc.hasDelay || delay != nil && *delay != tc.delay {
				t.Fatalf("retry hint = %v, want %v (present %t)", delay, tc.delay, tc.hasDelay)
			}
			if tc.name == "nested rate limit" {
				headers := upstream.Headers()
				if headers.Get("Retry-After") != "1" || headers.Get("Retry-After-Ms") != "15" {
					t.Fatalf("retry headers lost: %v", headers)
				}
				headers.Set("Retry-After", "999")
				if upstream.Headers().Get("Retry-After") != "1" {
					t.Fatal("returned headers mutated the error")
				}
			}
		})
	}
}

func TestUpstreamRetryAfter(t *testing.T) {
	now := time.Date(2026, 9, 29, 0, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name, ms, seconds string
		delay             time.Duration
		ok                bool
	}{
		{"milliseconds take precedence", "15", "1", 15 * time.Millisecond, true},
		{"invalid milliseconds fall back", "invalid", "2", 2 * time.Second, true},
		{"HTTP date", "", now.Add(3 * time.Second).Format(http.TimeFormat), 3 * time.Second, true},
		{"expired HTTP date", "", now.Add(-time.Second).Format(http.TimeFormat), 0, true},
		{"negative", "-1", "-1", 0, false},
		{"overflow", "999999999999999999999", "NaN", 0, false},
		{"absent", "", "", 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := make(http.Header)
			h.Set("Retry-After-Ms", tc.ms)
			h.Set("Retry-After", tc.seconds)
			delay := upstreamRetryAfter(h, now)
			if (delay != nil) != tc.ok || delay != nil && *delay != tc.delay {
				t.Fatalf("delay = %v, want %v (present %t)", delay, tc.delay, tc.ok)
			}
		})
	}
}

func TestTerminalErrorPrecedesToolConversion(t *testing.T) {
	for _, kind := range []string{"failed", "incomplete", "cancelled"} {
		for _, output := range []string{"partial_function", "partial_custom", "omitted"} {
			t.Run(kind+"/"+output, func(t *testing.T) {
				cache := &Cache{}
				_, bridge, err := Prepare([]byte(testRequest), t.Name(), "session", cache)
				if err != nil {
					t.Fatal(err)
				}
				item := nativeWeather()
				item["status"], item["arguments"] = "in_progress", `{"code":"partial`
				if output == "partial_custom" {
					item["type"], item["input"] = "custom_tool_call", "partial input"
				}
				items := []any{item}
				if output == "omitted" {
					items = nil
				}
				response := object{"status": kind, "output": items, "error": object{"code": "rate_limit_exceeded", "message": "TPM limit reached", "headers": object{"retry-after-ms": "15"}}}
				raw, _ := json.Marshal(object{"type": "response." + kind, "response": response})
				added, _ := json.Marshal(object{"type": "response.output_item.added", "item": item})
				var emitted bytes.Buffer
				err = bridge.Stream(strings.NewReader(fmt.Sprintf("data: %s\n\ndata: %s\n\n", added, raw)), func(event []byte) error { _, errWrite := emitted.Write(event); return errWrite })
				var upstream *Error
				if !errors.As(err, &upstream) || upstream.StatusCode() != 429 || err.Error() != string(raw) {
					t.Fatalf("upstream failure masked: %v", err)
				}
				if emitted.Len() != 0 || len(bridge.emitted) != 0 || len(cache.items) != 0 {
					t.Fatal("failed response emitted or cached a tool invocation")
				}
				if !bytes.Equal(bridge.LastEvent, raw) {
					t.Fatal("original terminal event missing from diagnostics")
				}
				direct, _ := json.Marshal(response)
				if converted, errResponse := bridge.Response(direct); errResponse == nil || converted != nil || errResponse.Error() != string(direct) {
					t.Fatalf("non-streaming failure masked: %s %v", converted, errResponse)
				}
			})
		}
	}
}

func TestIncompleteToolsWithoutUpstreamErrorStillRejected(t *testing.T) {
	_, bridge, err := Prepare([]byte(testRequest), t.Name(), "session", &Cache{})
	if err != nil {
		t.Fatal(err)
	}
	item := nativeWeather()
	item["status"], item["arguments"] = "in_progress", `{"code":"partial`
	raw, _ := json.Marshal(object{"type": "response.incomplete", "response": object{"status": "incomplete", "error": nil, "output": []any{item}}})
	err = bridge.Stream(strings.NewReader("data: "+string(raw)+"\n\n"), func([]byte) error { t.Fatal("incomplete tool emitted"); return nil })
	if err == nil || !strings.Contains(err.Error(), "incomplete_tools") {
		t.Fatalf("missing incomplete tool validation: %v", err)
	}
}
