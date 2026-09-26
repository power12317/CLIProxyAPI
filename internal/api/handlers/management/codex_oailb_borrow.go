package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

func (h *Handler) GetCodexOaiLBBorrow(c *gin.Context) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	c.Header("Cache-Control", "no-store")
	c.JSON(http.StatusOK, gin.H{"supported": true, "config": h.cfg.CodexHeaderDefaults.OaiLBBorrow})
}

func (h *Handler) PutCodexOaiLBBorrow(c *gin.Context) {
	var value config.CodexOaiLBBorrowConfig
	if c.ShouldBindJSON(&value) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	previous := h.cfg
	next := previous.CloneForRuntime()
	if value == (config.CodexOaiLBBorrowConfig{}) {
		next.CodexHeaderDefaults.OaiLBBorrow = nil
	} else {
		value.SourceURL = strings.TrimRight(strings.TrimSpace(value.SourceURL), "/")
		value.SourceAuthID = strings.TrimSpace(value.SourceAuthID)
		next.CodexHeaderDefaults.OaiLBBorrow = &value
		if err := config.ProtectCodexOaiLBSecret(next); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
		if _, err := config.CodexOaiLBManagementKey(next); err != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
			return
		}
	}
	h.cfg = next
	if !h.persistLocked(c) {
		h.cfg = previous
		return
	}
	helps.InvalidateCodexOaiLBBorrow(previous)
}

// BorrowCodexOaiLB exports only the selected credential's local routing cookie.
// Existing usage polling keeps working; a cache miss performs one on-demand refresh.
func (h *Handler) BorrowCodexOaiLB(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	var input struct {
		AuthID string `json:"auth_id"`
	}
	if c.ShouldBindJSON(&input) != nil || strings.TrimSpace(input.AuthID) == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "auth_id is required"})
		return
	}
	if h.authManager == nil {
		c.JSON(http.StatusOK, gin.H{"available": false})
		return
	}
	auth, ok := h.authManager.GetByID(input.AuthID)
	if !ok || auth.Disabled || !helps.CodexAuthUsesOAuthCookieJar(auth) {
		c.JSON(http.StatusOK, gin.H{"available": false})
		return
	}
	h.mu.Lock()
	cfg := h.cfg.CloneForRuntime()
	h.mu.Unlock()
	cookies, _ := helps.BorrowCodexRoutingCookies(c.Request.Context(), cfg, auth)
	expires, valid := helps.CodexOaiLBExpiry(cookies.OaiLB)
	if cookies.OaiLB == "" && cookies.CFLB == "" {
		c.JSON(http.StatusOK, gin.H{"available": false})
		return
	}
	// Recheck after acquisition so a disabled or replaced credential is not exported.
	latest, ok := h.authManager.GetByID(input.AuthID)
	if !ok || latest.Disabled || helps.CodexOwnerFingerprint(latest) != helps.CodexOwnerFingerprint(auth) {
		c.JSON(http.StatusOK, gin.H{"available": false})
		return
	}
	result := gin.H{"available": true, "value": cookies.OaiLB, "cflb": cookies.CFLB}
	if valid {
		result["expires_at"] = expires.UTC().Format(time.RFC3339)
		result["remaining_seconds"] = int64(time.Until(expires).Seconds())
	}
	c.JSON(http.StatusOK, result)
}
