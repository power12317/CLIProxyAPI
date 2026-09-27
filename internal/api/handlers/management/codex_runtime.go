package management

import (
	"context"
	"errors"
	"io"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
)

func (h *Handler) codexRuntimeConfig() *config.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.CloneForRuntime()
}

func runtimePreference(cfg *config.Config, credential bridge.Credential) bool {
	if cfg.Codex.Runtime.Enabled {
		if state, exists := codexshared.Get(credential.Metadata); exists {
			return state.Enabled
		}
	}
	if enabled, exists := cfg.Codex.Runtime.CredentialModes[credential.ID]; exists {
		return enabled
	}
	return true
}

func runtimeStatus(cfg *config.Config) (gin.H, error) {
	files, err := bridge.ListCredentials(cfg)
	if err != nil {
		return nil, err
	}
	credentials := make([]gin.H, 0, len(files))
	for _, credential := range files {
		metadata := credential.Metadata
		row := gin.H{"name": credential.ID, "label": credential.ID, "enabled": runtimePreference(cfg, credential), "owner": "cpa", "status": "cpa"}
		for _, key := range []string{"email", "label"} {
			if label, _ := metadata[key].(string); label != "" {
				row["label"] = label
			}
		}
		state, _ := codexshared.Get(metadata)
		disabled, _ := metadata["disabled"].(bool)
		if cfg.Codex.Runtime.Enabled && state.Enabled && !disabled {
			row["owner"], row["status"] = "codex", "codex"
		}
		if account, ok := metadata["account_id"].(string); ok {
			row["account_id"] = account
		}
		if token, _ := metadata["access_token"].(string); token == "" {
			row["status"] = "not_authorized"
		}
		if disabled {
			row["status"] = "disabled"
		}
		credentials = append(credentials, row)
	}
	return gin.H{"enabled": cfg.Codex.Runtime.Enabled, "credentials": credentials}, nil
}

func writeRuntimeStatus(c *gin.Context, cfg *config.Config) {
	status, err := runtimeStatus(cfg)
	if err != nil {
		runtimeHTTPError(c, err)
		return
	}
	c.JSON(http.StatusOK, status)
}

func (h *Handler) GetCodexRuntime(c *gin.Context) {
	writeRuntimeStatus(c, h.codexRuntimeConfig())
}

// saveRuntimeConfig persists panel preferences, applies them, then reloads CPA.
func (h *Handler) saveRuntimeConfig(c *gin.Context, update func(*config.Config) error, credentialIDs ...string) (*config.Config, bool) {
	h.mu.Lock()
	previous := h.cfg
	next := previous.CloneForRuntime()
	if err := update(next); err != nil {
		h.mu.Unlock()
		runtimeHTTPError(c, err)
		return nil, false
	}
	if err := next.ValidateCodexRuntime(); err != nil {
		h.mu.Unlock()
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return nil, false
	}
	h.cfg = next
	snapshot, ok := h.saveConfigAndSnapshotLocked(c)
	if !ok {
		h.cfg = previous
		h.mu.Unlock()
		return nil, false
	}
	h.mu.Unlock()
	errApply := bridge.ApplyConfig(c.Request.Context(), next, credentialIDs...)
	credentials, errList := bridge.ListCredentials(next)
	for _, credential := range credentials {
		h.syncRuntimeAuth(c.Request.Context(), credential.Path)
	}
	h.reloadConfigAfterManagementSave(c.Request.Context(), snapshot)
	if errApply != nil {
		runtimeHTTPError(c, errApply)
		return nil, false
	}
	if errList != nil {
		runtimeHTTPError(c, errList)
		return nil, false
	}
	return next, true
}

func (h *Handler) PutCodexRuntime(c *gin.Context) {
	var req struct {
		Enabled *bool `json:"enabled"`
	}
	if c.ShouldBindJSON(&req) != nil || req.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "enabled is required"})
		return
	}
	cfg, ok := h.saveRuntimeConfig(c, func(next *config.Config) error {
		if next.Codex.Runtime.Enabled && !*req.Enabled {
			credentials, err := bridge.ListCredentials(next)
			if err != nil {
				return err
			}
			if next.Codex.Runtime.CredentialModes == nil {
				next.Codex.Runtime.CredentialModes = make(map[string]bool)
			}
			for _, credential := range credentials {
				selected := runtimePreference(next, credential)
				if state, exists := codexshared.Get(credential.Metadata); exists {
					selected = state.Enabled
				}
				next.Codex.Runtime.CredentialModes[credential.ID] = selected
			}
		}
		next.Codex.Runtime.Enabled = *req.Enabled
		return nil
	})
	if ok {
		writeRuntimeStatus(c, cfg)
	}
}

func (h *Handler) syncRuntimeAuth(ctx context.Context, path string) {
	if h.authManager == nil {
		return
	}
	auth, err := h.buildAuthFromFileData(path, nil)
	if err == nil {
		_ = h.upsertAuthRecord(ctx, auth)
	}
}

func findRuntimeCredential(cfg *config.Config, name string) (bridge.Credential, bool, error) {
	credentials, err := bridge.ListCredentials(cfg)
	if err != nil {
		return bridge.Credential{}, false, err
	}
	for _, credential := range credentials {
		if credential.ID == name {
			return credential, true, nil
		}
	}
	return bridge.Credential{}, false, nil
}

func (h *Handler) setRuntimePreference(c *gin.Context, name string, enabled bool) (*config.Config, bool) {
	return h.saveRuntimeConfig(c, func(next *config.Config) error {
		if next.Codex.Runtime.CredentialModes == nil {
			next.Codex.Runtime.CredentialModes = make(map[string]bool)
		}
		next.Codex.Runtime.CredentialModes[name] = enabled
		return nil
	}, name)
}

func (h *Handler) SetCodexRuntimeCredential(c *gin.Context) {
	var req struct {
		Name    string `json:"name"`
		Enabled *bool  `json:"enabled"`
	}
	if c.ShouldBindJSON(&req) != nil || req.Name == "" || req.Enabled == nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "name and enabled are required"})
		return
	}
	_, found, err := findRuntimeCredential(h.codexRuntimeConfig(), req.Name)
	if err != nil {
		runtimeHTTPError(c, err)
		return
	}
	if !found {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Codex account is unavailable"})
		return
	}
	cfg, ok := h.setRuntimePreference(c, req.Name, *req.Enabled)
	if ok {
		writeRuntimeStatus(c, cfg)
	}
}

func runtimeHTTPError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	var typed interface{ StatusCode() int }
	if errors.As(err, &typed) {
		status = typed.StatusCode()
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

func (h *Handler) runtimeRPC(c *gin.Context, cfg *config.Config, credentialID string) (*bridge.Client, bool) {
	if !cfg.Codex.Runtime.Enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "Codex runtime integration is disabled"})
		return nil, false
	}
	credential, found, err := findRuntimeCredential(cfg, credentialID)
	if err != nil {
		runtimeHTTPError(c, err)
		return nil, false
	}
	if !found {
		c.JSON(http.StatusBadRequest, gin.H{"error": "Codex account is unavailable"})
		return nil, false
	}
	state, _ := codexshared.Get(credential.Metadata)
	disabled, _ := credential.Metadata["disabled"].(bool)
	if !state.Enabled || disabled {
		c.JSON(http.StatusConflict, gin.H{"error": "Codex account is disabled"})
		return nil, false
	}
	ctx, release := bridge.Track(c.Request.Context(), credential.Path)
	client, err := bridge.Dial(ctx, cfg.Codex.Runtime.Endpoint())
	if err != nil {
		release()
		runtimeHTTPError(c, err)
		return nil, false
	}
	client.OnClose(release)
	return client, true
}

func (h *Handler) StartCodexRuntimeLogin(c *gin.Context) {
	var req struct {
		Name string `json:"name"`
	}
	if err := c.ShouldBindJSON(&req); err != nil && !errors.Is(err, io.EOF) {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid OAuth request"})
		return
	}
	cfg := h.codexRuntimeConfig()
	if !cfg.Codex.Runtime.Enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "Codex runtime integration is disabled"})
		return
	}
	if req.Name == "" {
		req.Name = uuid.NewString() + ".json"
		if err := codexshared.Write(bridge.CredentialPath(cfg, req.Name), map[string]any{"type": "codex"}); err != nil {
			runtimeHTTPError(c, err)
			return
		}
	} else {
		credential, found, err := findRuntimeCredential(cfg, req.Name)
		if err != nil {
			runtimeHTTPError(c, err)
			return
		}
		if !found {
			c.JSON(http.StatusBadRequest, gin.H{"error": "Codex account is unavailable"})
			return
		}
		if disabled, _ := credential.Metadata["disabled"].(bool); disabled {
			c.JSON(http.StatusConflict, gin.H{"error": "Codex account is disabled"})
			return
		}
	}
	cfg, ok := h.setRuntimePreference(c, req.Name, true)
	if !ok {
		return
	}
	client, ok := h.runtimeRPC(c, cfg, req.Name)
	if !ok {
		return
	}
	defer client.Close()
	if err := client.Call("cpa/credential/reload", map[string]any{"credentialId": req.Name}, nil); err != nil {
		runtimeHTTPError(c, err)
		return
	}
	var result struct {
		LoginID string `json:"loginId"`
		AuthURL string `json:"authUrl"`
		State   string `json:"state"`
	}
	if err := client.Call("cpa/auth/login/start", map[string]any{"credentialId": req.Name}, &result); err != nil {
		runtimeHTTPError(c, err)
		return
	}
	RegisterOAuthSessionWithMetadata("codex-runtime:"+result.LoginID, "codex-runtime", map[string]any{"credential_id": req.Name})
	c.JSON(http.StatusOK, gin.H{"login_id": result.LoginID, "url": result.AuthURL, "state": result.State})
}

func (h *Handler) runtimeLoginRPC(c *gin.Context, loginID string) (*bridge.Client, string, bool) {
	cfg := h.codexRuntimeConfig()
	if !cfg.Codex.Runtime.Enabled {
		c.JSON(http.StatusConflict, gin.H{"error": "Codex runtime integration is disabled"})
		return nil, "", false
	}
	provider, _, _, metadata, _, ok := GetOAuthSessionDetails("codex-runtime:" + loginID)
	if !ok || provider != "codex-runtime" {
		c.JSON(http.StatusBadRequest, gin.H{"error": "unknown OAuth login"})
		return nil, "", false
	}
	credentialID, _ := metadata["credential_id"].(string)
	client, ok := h.runtimeRPC(c, cfg, credentialID)
	return client, credentialID, ok
}

func (h *Handler) CompleteCodexRuntimeLogin(c *gin.Context) {
	var req struct {
		LoginID     string `json:"login_id"`
		RedirectURL string `json:"redirect_url"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": "invalid OAuth callback"})
		return
	}
	client, credentialID, ok := h.runtimeLoginRPC(c, req.LoginID)
	if !ok {
		return
	}
	defer client.Close()
	var result map[string]any
	if err := client.Call("cpa/auth/login/callback", map[string]any{"loginId": req.LoginID, "redirectUrl": req.RedirectURL}, &result); err != nil {
		runtimeHTTPError(c, err)
		return
	}
	if result["status"] == "completed" {
		h.syncRuntimeAuth(c.Request.Context(), bridge.CredentialPath(h.codexRuntimeConfig(), credentialID))
	}
	c.JSON(http.StatusOK, result)
}

func (h *Handler) GetCodexRuntimeLoginStatus(c *gin.Context) {
	client, credentialID, ok := h.runtimeLoginRPC(c, c.Query("login_id"))
	if !ok {
		return
	}
	defer client.Close()
	var result map[string]any
	if err := client.Call("cpa/auth/login/status", map[string]any{"loginId": c.Query("login_id")}, &result); err != nil {
		runtimeHTTPError(c, err)
		return
	}
	if result["status"] == "completed" {
		h.syncRuntimeAuth(c.Request.Context(), bridge.CredentialPath(h.codexRuntimeConfig(), credentialID))
	}
	c.JSON(http.StatusOK, result)
}
