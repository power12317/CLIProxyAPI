package logging

import (
	"strings"

	"github.com/gin-gonic/gin"
)

const codexTurnStateLogKey = "__codex_turn_state_log__"

const codexOaiLBNodeLogKey = "__codex_oailb_node__"

func SetCodexOaiLBNode(c *gin.Context, node string) {
	if c != nil {
		c.Set(codexOaiLBNodeLogKey, node)
	}
}

func CodexOaiLBNode(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, _ := c.Get(codexOaiLBNodeLogKey)
	node, _ := value.(string)
	return node
}

// CodexTicketProbeLogField selects the ticket probe marker instead of a source location.
const CodexTicketProbeLogField = "codex_ticket_probe"

// CodexTurnStateLogFields contains the fields appended to the Gin access log
// for a Codex request. The request ID remains in the access log's standard ID
// column.
type CodexTurnStateLogFields struct {
	AuthFile             string
	SessionID            string
	TurnID               string
	RequestedModel       string
	ResponseModel        string
	RequestTurnStateLen  int
	ResponseTurnStateLen int
}

// ShortCodexIdentifier returns the UUID's first segment for log display. The
// full identifier remains available to request processing and usage records.
func ShortCodexIdentifier(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	if index := strings.IndexByte(value, '-'); index > 0 {
		value = value[:index]
	}
	if len(value) > 8 {
		return value[:8]
	}
	return value
}

// SetCodexTurnStateLogFields stores the latest Codex turn-state statistics on
// the request so GinLogrusLogger can append them to the access-log line.
func SetCodexTurnStateLogFields(c *gin.Context, fields CodexTurnStateLogFields) {
	if c == nil {
		return
	}
	c.Set(codexTurnStateLogKey, fields)
}

// UpdateCodexTurnStateLogModels updates only the model fields already attached
// to a Codex request. The access log keeps the request and response models on
// the same line as the existing turn-state fields.
func UpdateCodexTurnStateLogModels(c *gin.Context, requestedModel, responseModel string) {
	if c == nil {
		return
	}
	fields, exists := codexTurnStateLogFields(c)
	if !exists {
		return
	}
	fields.RequestedModel = requestedModel
	fields.ResponseModel = responseModel
	c.Set(codexTurnStateLogKey, fields)
}

// CodexTurnStateLogFieldsForContext returns the latest Codex request fields
// stored on the Gin context.
func CodexTurnStateLogFieldsForContext(c *gin.Context) (CodexTurnStateLogFields, bool) {
	return codexTurnStateLogFields(c)
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
