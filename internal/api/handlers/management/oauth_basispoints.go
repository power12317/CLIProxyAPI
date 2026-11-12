package management

import (
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// GetBasispointsStatus never refreshes credentials or contacts upstream.
func (h *Handler) GetBasispointsStatus(c *gin.Context) {
	name := strings.TrimSpace(c.Query("name"))
	if name == "" {
		name = strings.TrimSpace(c.Query("id"))
	}
	index := firstNonEmpty(c.Query("auth_index"), c.Query("authIndex"))
	if name == "" && index == "" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name or auth_index is required"})
		return
	}
	if h == nil || h.authManager == nil {
		c.JSON(http.StatusServiceUnavailable, gin.H{"error": "auth manager unavailable"})
		return
	}
	var target *coreauth.Auth
	if name != "" {
		target, _ = h.lookupAuthFile(name, index)
	} else {
		target = h.authByIndex(index)
	}
	if target == nil {
		c.JSON(http.StatusNotFound, gin.H{"error": "auth file not found"})
		return
	}
	if !coreauth.IsCodexAuthRecord(target) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Basispoints authorization requires a Codex account"})
		return
	}
	status := basispointsStatusFields(target)
	status["bind_supported"] = siwcBindEligible(target)
	c.JSON(http.StatusOK, status)
}

func basispointsStatusFields(auth *coreauth.Auth) gin.H {
	grant := coreauth.BasispointsGrant(auth)
	out := gin.H{"status": coreauth.BasispointsGrantStatus(auth, time.Now()), "bound": len(grant) > 0, "use_basispoints": coreauth.IsCodexUseBasispoints(auth)}
	if expiry, _ := grant["expired"].(string); expiry != "" {
		out["expires_at"] = expiry
	}
	return out
}
