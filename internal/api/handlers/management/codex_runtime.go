package management

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
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

func (h *Handler) runtimeRPC(ctx context.Context, cfg *config.Config, credentialID string) (*bridge.Client, error) {
	if !cfg.Codex.Runtime.Enabled {
		return nil, &bridge.Error{Status: http.StatusConflict, Message: "Codex runtime integration is disabled"}
	}
	credential, found, err := findRuntimeCredential(cfg, credentialID)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, &bridge.Error{Status: http.StatusBadRequest, Message: "Codex account is unavailable"}
	}
	state, _ := codexshared.Get(credential.Metadata)
	disabled, _ := credential.Metadata["disabled"].(bool)
	if !state.Enabled || disabled {
		return nil, &bridge.Error{Status: http.StatusConflict, Message: "Codex account is disabled"}
	}
	ctx, release := bridge.Track(ctx, credential.Path)
	client, err := bridge.Dial(ctx, cfg.Codex.Runtime.Endpoint())
	if err != nil {
		release()
		return nil, err
	}
	client.OnClose(release)
	return client, nil
}

// requestCodexRuntimeToken implements the existing Codex OAuth entry using the master.
func (h *Handler) requestCodexRuntimeToken(c *gin.Context, target *coreauth.Auth, clientSystem string) {
	cfg := h.codexRuntimeConfig()
	credentialID := ""
	if target == nil {
		credentialID = uuid.NewString() + ".json"
		metadata := map[string]any{"type": "codex", "auth_kind": coreauth.AuthKindOAuth, "codex_client_system": clientSystem}
		if err := codexshared.Write(bridge.CredentialPath(cfg, credentialID), metadata); err != nil {
			runtimeHTTPError(c, err)
			return
		}
	} else {
		credentialID = target.ID
		credential, found, err := findRuntimeCredential(cfg, credentialID)
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
	cfg, ok := h.setRuntimePreference(c, credentialID, true)
	if !ok {
		return
	}
	client, err := h.runtimeRPC(c.Request.Context(), cfg, credentialID)
	if err != nil {
		runtimeHTTPError(c, err)
		return
	}
	defer client.Close()
	if err := client.Call("cpa/credential/reload", map[string]any{"credentialId": credentialID}, nil); err != nil {
		runtimeHTTPError(c, err)
		return
	}
	var result struct {
		LoginID string `json:"loginId"`
		AuthURL string `json:"authUrl"`
		State   string `json:"state"`
	}
	if err := client.Call("cpa/auth/login/start", map[string]any{"credentialId": credentialID}, &result); err != nil {
		runtimeHTTPError(c, err)
		return
	}
	authURL, err := url.Parse(result.AuthURL)
	if err != nil || ValidateOAuthState(result.State) != nil || result.LoginID == "" {
		c.JSON(http.StatusBadGateway, gin.H{"error": "invalid Codex OAuth response"})
		return
	}
	RegisterOAuthSessionWithMetadata(result.State, "codex", map[string]any{
		"credential_id":    credentialID,
		"new_credential":   target == nil,
		"runtime_login_id": result.LoginID,
		"redirect_uri":     authURL.Query().Get("redirect_uri"),
	})
	c.JSON(http.StatusOK, gin.H{"status": "ok", "url": result.AuthURL, "state": result.State, "client_system": clientSystem})
}

type runtimeOAuthResult struct {
	Status string `json:"status"`
	Error  string `json:"error"`
}

func runtimeOAuthLoginID(metadata map[string]any) string {
	loginID, _ := metadata["runtime_login_id"].(string)
	return loginID
}

func (h *Handler) callRuntimeOAuth(ctx context.Context, metadata map[string]any, method string, params map[string]any) (runtimeOAuthResult, error) {
	credentialID, _ := metadata["credential_id"].(string)
	client, err := h.runtimeRPC(ctx, h.codexRuntimeConfig(), credentialID)
	if err != nil {
		return runtimeOAuthResult{}, err
	}
	defer client.Close()
	params["loginId"] = runtimeOAuthLoginID(metadata)
	var result runtimeOAuthResult
	err = client.Call(method, params, &result)
	return result, err
}

func runtimeOAuthError(result runtimeOAuthResult) string {
	if message := strings.TrimSpace(result.Error); message != "" {
		return message
	}
	return "Codex authentication failed"
}

func (h *Handler) pollRuntimeOAuth(c *gin.Context, state string, metadata map[string]any) {
	result, err := h.callRuntimeOAuth(c.Request.Context(), metadata, "cpa/auth/login/status", map[string]any{})
	if err != nil {
		SetOAuthSessionError(state, err.Error())
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": err.Error()})
		return
	}
	switch result.Status {
	case "completed":
		if !h.completeRuntimeOAuth(c, state, metadata) {
			return
		}
		c.JSON(http.StatusOK, gin.H{"status": "ok"})
	case "error":
		message := runtimeOAuthError(result)
		SetOAuthSessionError(state, message)
		c.JSON(http.StatusOK, gin.H{"status": "error", "error": message})
	default:
		c.JSON(http.StatusOK, gin.H{"status": "wait"})
	}
}

func (h *Handler) callbackRuntimeOAuth(c *gin.Context, state string, metadata map[string]any, redirectURL, code, oauthError string) {
	if strings.TrimSpace(redirectURL) == "" {
		base, _ := metadata["redirect_uri"].(string)
		callback, err := url.Parse(base)
		if err != nil || base == "" {
			c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": "OAuth redirect URI is unavailable"})
			return
		}
		query := callback.Query()
		query.Set("state", state)
		if code != "" {
			query.Set("code", code)
		}
		if oauthError != "" {
			query.Set("error", oauthError)
		}
		callback.RawQuery = query.Encode()
		redirectURL = callback.String()
	}
	result, err := h.callRuntimeOAuth(c.Request.Context(), metadata, "cpa/auth/login/callback", map[string]any{"redirectUrl": redirectURL})
	if err != nil {
		SetOAuthSessionError(state, err.Error())
		status := http.StatusInternalServerError
		var typed interface{ StatusCode() int }
		if errors.As(err, &typed) {
			status = typed.StatusCode()
		}
		c.JSON(status, gin.H{"status": "error", "error": err.Error()})
		return
	}
	if result.Status == "error" {
		message := runtimeOAuthError(result)
		SetOAuthSessionError(state, message)
		c.JSON(http.StatusBadRequest, gin.H{"status": "error", "error": message})
		return
	}
	if result.Status == "completed" {
		if !h.completeRuntimeOAuth(c, state, metadata) {
			return
		}
	}
	c.JSON(http.StatusOK, gin.H{"status": "ok"})
}
