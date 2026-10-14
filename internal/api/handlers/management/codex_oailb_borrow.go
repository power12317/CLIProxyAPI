package management

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

// GetCodexOaiLBBorrow preserves the configuration capability contract consumed by
// CPAMP. Supported describes the API, independently of the ticket master switch.
func (h *Handler) GetCodexOaiLBBorrow(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	var value *config.CodexOaiLBBorrowConfig
	if current := h.cfg.CodexHeaderDefaults.OaiLBBorrow; current != nil {
		copy := *current
		// Normal loading and all setters encrypt the key. Never expose plaintext
		// supplied by an in-process caller that bypassed those entrypoints.
		if !strings.HasPrefix(copy.SourceManagementKey, "enc:v1:") {
			copy.SourceManagementKey = ""
		}
		value = &copy
	}
	c.JSON(http.StatusOK, gin.H{"supported": true, "config": value})
}

// PutCodexOaiLBBorrow uses the original flat object and status envelope. Empty
// objects and null disable borrowing; persisted v8 layouts remain v8 layouts.
func (h *Handler) PutCodexOaiLBBorrow(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 64<<10)
	var value *config.CodexOaiLBBorrowConfig
	decoder := json.NewDecoder(c.Request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	if value != nil {
		value.SourceInstanceID = strings.TrimSpace(value.SourceInstanceID)
		value.SourceURL = strings.TrimRight(strings.TrimSpace(value.SourceURL), "/")
		value.SourceManagementKey = strings.TrimSpace(value.SourceManagementKey)
		value.SourceAuthID = strings.TrimSpace(value.SourceAuthID)
		value.SourceAuthFile = strings.TrimSpace(value.SourceAuthFile)
		if *value == (config.CodexOaiLBBorrowConfig{}) {
			value = nil
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	previous := h.cfg
	next := previous.CloneForRuntime()
	next.CodexHeaderDefaults.OaiLBBorrow = value
	if value != nil {
		if err := config.ProtectCodexOaiLBSecret(next); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "invalid borrowing configuration or unavailable encryption key"})
			return
		}
		if _, err := config.CodexOaiLBManagementKey(next); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": "cannot decrypt source management key"})
			return
		}
	}
	h.cfg = next
	if !h.persistLocked(c) {
		h.cfg = previous
	}
}
