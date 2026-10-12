package logging

import (
	"strings"

	"github.com/gin-gonic/gin"
)

const codexTurnStateLogKey = "__codex_turn_state_log__"
const codexTurnStateLogModelsKey = "__codex_turn_state_log_models__"

type codexTurnStateLogModels struct {
	requested string
	response  string
}

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

// SetCodexTurnStateLogFields initializes the access-log fields for an upstream
// attempt, clearing any response model left by a previous attempt.
func SetCodexTurnStateLogFields(c *gin.Context, fields CodexTurnStateLogFields) {
	if c == nil {
		return
	}
	c.Set(codexTurnStateLogKey, fields)
	c.Set(codexTurnStateLogModelsKey, codexTurnStateLogModels{fields.RequestedModel, fields.ResponseModel})
}

// UpdateCodexTurnStateLogFields updates turn-state statistics without replacing
// the independently reported models, including during final deferred updates.
func UpdateCodexTurnStateLogFields(c *gin.Context, fields CodexTurnStateLogFields) {
	if c != nil {
		c.Set(codexTurnStateLogKey, fields)
	}
}

// UpdateCodexTurnStateLogModels updates only the model fields already attached
// to a Codex request. The access log keeps the request and response models on
// the same line as the existing turn-state fields.
func UpdateCodexTurnStateLogModels(c *gin.Context, requestedModel, responseModel string) {
	if c == nil {
		return
	}
	_, exists := c.Get(codexTurnStateLogKey)
	if !exists {
		return
	}
	c.Set(codexTurnStateLogModelsKey, codexTurnStateLogModels{requestedModel, responseModel})
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
	if modelsValue, exists := c.Get(codexTurnStateLogModelsKey); exists {
		if models, valid := modelsValue.(codexTurnStateLogModels); valid {
			fields.RequestedModel = models.requested
			fields.ResponseModel = models.response
		}
	}
	return fields, ok
}
