package basispoints

import (
	"encoding/json"
	"net/http"
	"strings"
	"time"
)

// Error preserves upstream status and JSON while identifying request-only failures.
type Error struct {
	Status int
	Body   string

	headers    http.Header
	retryAfter *time.Duration
}

func (e *Error) Error() string              { return e.Body }
func (e *Error) StatusCode() int            { return e.Status }
func (e *Error) Headers() http.Header       { return e.headers.Clone() }
func (e *Error) RetryAfter() *time.Duration { return e.retryAfter }
func (e *Error) IsRequestScoped() bool {
	return e.Status == 400 || e.Status == 403 || e.Status == 404 || e.Status == 422 || e.Status == 502
}

// StreamError retains the original event, including nested upstream error details.
func StreamError(raw []byte) error {
	value, _ := decode(raw)
	response, _ := value["response"].(object)
	detail, _ := response["error"].(object)
	if detail == nil {
		detail, _ = value["error"].(object)
	}
	status := upstreamErrorStatus(value, detail)
	headers := make(http.Header)
	for _, source := range []object{value, response, detail} {
		mapped, _ := source["headers"].(object)
		for name, value := range mapped {
			name = http.CanonicalHeaderKey(strings.TrimSpace(name))
			if name != "Retry-After" && name != "Retry-After-Ms" {
				continue
			}
			switch value := value.(type) {
			case string:
				headers.Set(name, strings.TrimSpace(value))
			case json.Number:
				headers.Set(name, value.String())
			}
		}
	}
	err := &Error{Status: status, Body: string(raw), headers: headers}
	if status == http.StatusTooManyRequests {
		err.retryAfter = upstreamRetryAfter(headers, time.Now())
	}
	return err
}

func upstreamErrorStatus(value, detail object) int {
	for _, source := range []object{value, detail} {
		for _, field := range []string{"status", "status_code"} {
			if number, ok := source[field].(json.Number); ok {
				if status, err := number.Int64(); err == nil && status >= 400 && status <= 599 {
					return int(status)
				}
			}
		}
	}
	code := strings.ToLower(strings.TrimSpace(stringValue(detail["code"])))
	kind := strings.ToLower(strings.TrimSpace(stringValue(detail["type"])))
	if kind == "rate_limit_error" || code == "rate_limit_exceeded" || code == "slow_down" {
		return http.StatusTooManyRequests
	}
	return http.StatusBadGateway
}

func upstreamRetryAfter(headers http.Header, now time.Time) *time.Duration {
	// Prefer the upstream millisecond hint when both representations are present.
	for _, hint := range []struct{ name, unit string }{{"Retry-After-Ms", "ms"}, {"Retry-After", "s"}} {
		if raw := headers.Get(hint.name); raw != "" {
			if delay, err := time.ParseDuration(raw + hint.unit); err == nil && delay >= 0 {
				return &delay
			}
		}
	}
	if deadline, err := http.ParseTime(headers.Get("Retry-After")); err == nil {
		delay := deadline.Sub(now)
		if delay < 0 {
			delay = 0
		}
		return &delay
	}
	return nil
}
