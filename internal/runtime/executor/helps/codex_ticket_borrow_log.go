package helps

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// This value is request-local. Cached tickets must not retain request contexts.
type codexBorrowLog struct {
	requestID, authFile, sessionID, turnID, model string
}

func newCodexBorrowLog(ctx context.Context, auth *cliproxyauth.Auth, model string) codexBorrowLog {
	l := codexBorrowLog{requestID: logging.GetRequestID(ctx), model: model}
	if c := ginContextFrom(ctx); c != nil {
		if l.requestID == "" {
			l.requestID = logging.GetGinRequestID(c)
		}
		if fields, ok := logging.CodexTurnStateLogFieldsForContext(c); ok {
			l.sessionID, l.turnID = fields.SessionID, fields.TurnID
		}
	}
	if l.requestID == "" {
		l.requestID, _ = logging.GenerateRequestID()
	}
	if auth != nil {
		l.authFile = strings.TrimSpace(auth.FileName)
		if l.authFile == "" {
			l.authFile = auth.ID
		}
		l.authFile = filepath.Base(l.authFile)
	}
	return l
}

func (l codexBorrowLog) entry() *log.Entry {
	return log.WithFields(log.Fields{
		logging.CodexTicketBorrowLogField: true,
		"request_id":                      l.requestID, "auth_file": borrowLogText(l.authFile),
		"session_id": borrowLogText(logging.ShortCodexIdentifier(l.sessionID)),
		"turn_id":    borrowLogText(logging.ShortCodexIdentifier(l.turnID)),
	})
}

func (l codexBorrowLog) event(level log.Level, stage, message string) {
	l.entry().Logf(level, "%s | %s | %s", stage, borrowLogText(l.model), message)
}

func (l codexBorrowLog) request(stage string, req *http.Request, resp *http.Response, responseLength int, elapsed time.Duration, detail string, err error) {
	status, method, target := "---", http.MethodPost, ""
	requestLength := 0
	level := log.InfoLevel
	if req != nil {
		method = req.Method
		target = codexBorrowLogURL(req.URL)
		requestLength = len(codexTurnHeaderValue(req.Header, CodexTurnStateTicketHeader))
	}
	if resp != nil {
		status = strconv.Itoa(resp.StatusCode)
		if resp.StatusCode >= 500 {
			level = log.ErrorLevel
		} else if resp.StatusCode >= 400 {
			level = log.WarnLevel
		}
	}
	if err != nil {
		if level == log.InfoLevel {
			level = log.WarnLevel
		}
		detail += " | " + codexBorrowFailureReason(err)
	}
	l.entry().Logf(level, "%s %s | %.3fs | %s/- | %d/%d | %s %q%s",
		stage, status, elapsed.Truncate(time.Millisecond).Seconds(), borrowLogText(l.model),
		requestLength, responseLength, method, target, detail)
}

func codexBorrowLogURL(u *url.URL) string {
	if u == nil {
		return ""
	}
	// Source URLs may include user info or query secrets. Never serialize them.
	return u.Scheme + "://" + u.Host + u.EscapedPath()
}

func codexBorrowSourceURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "invalid URL"
	}
	return codexBorrowLogURL(u)
}

func borrowLogText(value string) string {
	quoted := strconv.Quote(value)
	return quoted[1 : len(quoted)-1]
}

func codexBorrowFailureReason(err error) string {
	var failure *CodexTicketBorrowError
	if errors.As(err, &failure) {
		return failure.reason
	}
	return codexBorrowIOReason(err)
}

// Transport errors can contain URLs or response bodies; only classify them.
func codexBorrowIOReason(err error) string {
	var networkError net.Error
	switch {
	case errors.Is(err, context.Canceled):
		return "request canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "deadline exceeded"
	case errors.As(err, &networkError) && networkError.Timeout():
		return "network timeout"
	case errors.Is(err, io.ErrUnexpectedEOF):
		return "unexpected EOF"
	case errors.Is(err, io.EOF):
		return "EOF"
	default:
		return fmt.Sprintf("I/O failure (%T)", err)
	}
}

func codexBorrowStageError(stage string, err error) error {
	return borrowError(stage + ": " + codexBorrowFailureReason(err))
}

func codexBorrowRetryReason(retryAt, now time.Time, reason, requestID string) string {
	seconds := max(int64((retryAt.Sub(now)+time.Second-1)/time.Second), 0)
	return fmt.Sprintf("acquisition retry is deferred for %d seconds; last failure: %s; acquisition request: %s",
		seconds, reason, logging.ShortRequestID(requestID))
}

func codexBorrowTicketDetail(ticket *CodexBorrowedTicket) string {
	return fmt.Sprintf("ticket=%d | remaining=%ds",
		len(ticket.State),
		max(int64(ticket.Route.deadline(ticket.ExpiresAt).Sub(time.Now())/time.Second), 0))
}

func codexBorrowResponseDetail(state string, resp *http.Response) string {
	if resp == nil {
		return ""
	}
	returned := strings.TrimSpace(resp.Header.Get(CodexTurnStateTicketHeader))
	comparison := "absent"
	if returned != "" {
		comparison = "same"
		if returned != state {
			comparison = "changed"
		}
	}
	return " | ticket=" + comparison
}

func codexBorrowRouteFailure(route CodexTicketRoute, now time.Time) string {
	for _, pair := range []struct {
		name   string
		cookie CodexRouteCookie
	}{{"__cflb", route.CFLB}, {"__oailb", route.OaiLB}} {
		if pair.cookie.Value == "" {
			return pair.name + " cookie is missing or deleted"
		}
		if !pair.cookie.ExpiresAt.IsZero() && !now.Before(pair.cookie.ExpiresAt) {
			return pair.name + " cookie has expired"
		}
		if (&http.Cookie{Name: "route", Value: pair.cookie.Value}).Valid() != nil {
			return pair.name + " cookie is malformed"
		}
	}
	return ""
}

func (b *CodexBorrowedTicket) businessLog(req *http.Request) codexBorrowLog {
	ctx := context.Background()
	if req != nil {
		ctx = req.Context()
	}
	trace := newCodexBorrowLog(ctx, nil, b.model)
	trace.authFile = b.authFile
	return trace
}
