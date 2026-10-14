package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
)

// BorrowCodexOaiLB extends the original borrowing operation with a complete
// model-scoped snapshot, without performing upstream I/O.
func (h *Handler) BorrowCodexOaiLB(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var request struct {
		AuthID string `json:"auth_id"`
		Model  string `json:"model"`
	}
	c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 4096)
	if err := c.ShouldBindJSON(&request); err != nil || strings.TrimSpace(request.AuthID) == "" || strings.TrimSpace(request.Model) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id_and_model_required"})
		return
	}
	h.mu.Lock()
	enabled := h.cfg != nil && h.cfg.Codex.EffectiveTurnStateTicket().Enabled && h.cfg.CodexHeaderDefaults.OaiLBBorrow == nil
	h.mu.Unlock()
	if !enabled || h.authManager == nil {
		c.JSON(http.StatusOK, helps.CodexOaiLBBorrowResponse{})
		return
	}
	auth, ok := h.authManager.GetByID(strings.TrimSpace(request.AuthID))
	if !ok {
		c.JSON(http.StatusOK, helps.CodexOaiLBBorrowResponse{})
		return
	}
	ticket, err := helps.ExportCodexSourceTicket(auth, strings.TrimSpace(request.Model), time.Now())
	if err != nil {
		c.JSON(http.StatusOK, helps.CodexOaiLBBorrowResponse{})
		return
	}
	latest, exists := h.authManager.GetByID(auth.ID)
	if !exists || latest.Disabled || latest.RegistrationEpoch != auth.RegistrationEpoch || helps.CodexOwnerFingerprint(latest) != helps.CodexOwnerFingerprint(auth) {
		c.JSON(http.StatusOK, helps.CodexOaiLBBorrowResponse{})
		return
	}
	c.JSON(http.StatusOK, helps.NewCodexOaiLBBorrowResponse(ticket, time.Now()))
}
