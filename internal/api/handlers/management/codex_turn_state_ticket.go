package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
)

// GetCodexTurnStateTicket exposes the live ticket policy and redacted per-auth
// readiness to management clients.
func (h *Handler) GetCodexTurnStateTicket(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	policy := h.cfg.Codex.EffectiveTurnStateTicket()
	response := gin.H{
		"enabled":                  policy.Enabled,
		"target_length":            policy.TargetLength,
		"ttl_seconds":              policy.TTLSeconds,
		"refresh_before_seconds":   policy.RefreshBeforeSeconds,
		"probe_interval_seconds":   policy.ProbeIntervalSeconds,
		"attempt_timeout_seconds":  policy.AttemptTimeoutSeconds,
		"fail_closed":              policy.FailClosed,
		"models":                   policy.Models,
		"harvest_proxy_url":        helps.MaskCodexTurnStateTicketHarvestProxyURL(policy.HarvestProxyURL),
		"harvest_proxy_configured": strings.TrimSpace(policy.HarvestProxyURL) != "",
		"accounts":                 []any{},
	}
	if h.authManager == nil {
		c.JSON(http.StatusOK, response)
		return
	}
	accounts := make([]gin.H, 0)
	for _, auth := range h.authManager.List() {
		statuses := helps.CodexTurnStateTicketStatuses(auth, policy, time.Now())
		if len(statuses) == 0 {
			continue
		}
		accounts = append(accounts, gin.H{"id": auth.ID, "name": auth.FileName, "tickets": statuses})
	}
	response["accounts"] = accounts
	c.JSON(http.StatusOK, response)
}

// PutCodexTurnStateTicket updates the policy while preserving unrelated config
// fields and comments through the normal management persistence path.
func (h *Handler) PutCodexTurnStateTicket(c *gin.Context) {
	if h == nil || h.cfg == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "config unavailable"})
		return
	}
	var req struct {
		Enabled               *bool    `json:"enabled"`
		TargetLength          *int     `json:"target_length"`
		TTLSeconds            *int     `json:"ttl_seconds"`
		RefreshBeforeSeconds  *int     `json:"refresh_before_seconds"`
		ProbeIntervalSeconds  *int     `json:"probe_interval_seconds"`
		AttemptTimeoutSeconds *int     `json:"attempt_timeout_seconds"`
		FailClosed            *bool    `json:"fail_closed"`
		HarvestProxyURL       *string  `json:"harvest_proxy_url"`
		Models                []string `json:"models"`
	}
	if errBind := c.ShouldBindJSON(&req); errBind != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid request body"})
		return
	}
	policy := &h.cfg.Codex.TurnStateTicket
	if req.Enabled != nil {
		policy.Enabled = *req.Enabled
	}
	if req.TargetLength != nil {
		policy.TargetLength = *req.TargetLength
	}
	if req.TTLSeconds != nil {
		policy.TTLSeconds = *req.TTLSeconds
	}
	if req.RefreshBeforeSeconds != nil {
		policy.RefreshBeforeSeconds = *req.RefreshBeforeSeconds
	}
	if req.ProbeIntervalSeconds != nil {
		policy.ProbeIntervalSeconds = *req.ProbeIntervalSeconds
	}
	if req.AttemptTimeoutSeconds != nil {
		policy.AttemptTimeoutSeconds = *req.AttemptTimeoutSeconds
	}
	if req.FailClosed != nil {
		policy.FailClosed = *req.FailClosed
	}
	if req.HarvestProxyURL != nil && !helps.IsMaskedCodexTurnStateTicketHarvestProxyURL(*req.HarvestProxyURL) {
		if errProxy := helps.ValidateCodexTurnStateTicketHarvestProxyURL(*req.HarvestProxyURL); errProxy != nil {
			c.JSON(http.StatusBadRequest, gin.H{"error": errProxy.Error()})
			return
		}
		policy.HarvestProxyURL = strings.TrimSpace(*req.HarvestProxyURL)
	}
	if req.Models != nil {
		policy.Models = req.Models
	}
	if !h.persist(c) {
		return
	}
}
