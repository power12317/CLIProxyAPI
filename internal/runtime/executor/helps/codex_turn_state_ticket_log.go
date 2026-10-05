package helps

import (
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/logging"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

// logCodexTurnStateTicketProbe records the wire lengths before ticket validation.
func logCodexTurnStateTicketProbe(auth *cliproxyauth.Auth, req *http.Request, model, sessionID, turnID string, resp *http.Response, elapsed time.Duration, errRequest error) {
	authFile := strings.TrimSpace(auth.FileName)
	if authFile == "" {
		authFile = auth.ID
	}
	status := "---"
	responseLength := 0
	level := log.InfoLevel
	if resp != nil {
		status = strconv.Itoa(resp.StatusCode)
		responseLength = len(strings.TrimSpace(codexTurnHeaderValue(resp.Header, CodexTurnStateTicketHeader)))
		switch {
		case resp.StatusCode >= http.StatusInternalServerError:
			level = log.ErrorLevel
		case resp.StatusCode >= http.StatusBadRequest:
			level = log.WarnLevel
		}
	} else if errRequest != nil {
		level = log.WarnLevel
	}
	requestID, errGenerate := logging.GenerateRequestID()
	if errGenerate != nil {
		log.WithError(errGenerate).Warn("codex ticket probe: generate request ID")
	}
	log.WithFields(log.Fields{
		logging.CodexTicketProbeLogField: true,
		"request_id":                     requestID,
		"auth_file":                      filepath.Base(authFile),
		"session_id":                     logging.ShortCodexIdentifier(sessionID),
		"turn_id":                        logging.ShortCodexIdentifier(turnID),
	}).Logf(level, "%s | %.3fs | %s/- | %d/%d | %s %q",
		status, elapsed.Truncate(time.Millisecond).Seconds(), model,
		len(codexTurnHeaderValue(req.Header, CodexTurnStateTicketHeader)), responseLength,
		req.Method, req.URL.EscapedPath())
}
