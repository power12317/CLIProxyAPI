package logging

import "github.com/gin-gonic/gin"

const codexTurnStateLogKey = "__codex_turn_state_log__"

// CodexTurnStateLogFields contains the fields appended to the Gin access log
// for a Codex request. The request ID remains in the access log's standard ID
// column.
type CodexTurnStateLogFields struct {
	Email                string
	SessionID            string
	TurnID               string
	ResponseTurnStateLen int
}

// SetCodexTurnStateLogFields stores the latest Codex turn-state statistics on
// the request so GinLogrusLogger can append them to the access-log line.
func SetCodexTurnStateLogFields(c *gin.Context, fields CodexTurnStateLogFields) {
	if c == nil {
		return
	}
	c.Set(codexTurnStateLogKey, fields)
}

func codexTurnStateLogFields(c *gin.Context) (CodexTurnStateLogFields, bool) {
	if c == nil {
		return CodexTurnStateLogFields{}, false
	}
	value, exists := c.Get(codexTurnStateLogKey)
	if !exists {
		return CodexTurnStateLogFields{}, false
	}
	fields, ok := value.(CodexTurnStateLogFields)
	return fields, ok
}
