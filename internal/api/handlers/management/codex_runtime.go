package management

import (
	"context"
	"errors"
	"net/http"
	"os"
	"path/filepath"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/codexruntime"
)

func (h *Handler) codexRuntimeConfig() *config.Config {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.cfg.CloneForRuntime()
}

func runtimeStatus(cfg *config.Config) gin.H {
	workers := make([]gin.H, 0, len(cfg.Codex.Runtime.Workers))
	credentials := make([]gin.H, 0, len(cfg.Codex.Runtime.Workers))
	for _, w := range cfg.Codex.Runtime.Workers {
		workers = append(workers, gin.H{"id": w.ID, "url": w.URL, "auth_file": w.AuthFile, "token_configured": w.Token != "", "models": append([]string{}, w.Models...), "disabled": w.Disabled})
		if w.AuthFile == "" {
			continue
		}
		row := gin.H{"name": w.AuthFile, "worker_id": w.ID, "enabled": false, "owner": "cpa", "status": "missing"}
		if metadata, err := codexshared.Read(bridge.CredentialPath(cfg, w)); err == nil {
			state, _ := codexshared.Get(metadata)
			row["enabled"], row["owner"], row["status"] = state.Enabled, state.Owner, state.Owner
			if account, ok := metadata["account_id"].(string); ok {
				row["account_id"] = account
			}
			if token, _ := metadata["access_token"].(string); token == "" {
				row["status"] = "not_authorized"
			}
			if disabled, _ := metadata["disabled"].(bool); disabled || w.Disabled {
				row["status"] = "disabled"
			}
		}
		credentials = append(credentials, row)
	}
	return gin.H{"enabled": cfg.Codex.Runtime.Enabled, "workers": workers, "credentials": credentials}
}

func (h *Handler) GetCodexRuntime(c *gin.Context) {
	c.JSON(http.StatusOK, runtimeStatus(h.codexRuntimeConfig()))
}

func (h *Handler) PutCodexRuntime(c *gin.Context) {
	var req struct {
		Enabled *bool `json:"enabled"`
		Workers *[]struct {
			ID       string   `json:"id"`
			URL      string   `json:"url"`
			AuthFile string   `json:"auth_file"`
			Token    *string  `json:"token"`
			Models   []string `json:"models"`
			Disabled bool     `json:"disabled"`
		} `json:"workers"`
	}
	if err := c.ShouldBindJSON(&req); err != nil {
		c.JSON(400, gin.H{"error": "invalid runtime configuration"})
		return
	}
	h.mu.Lock()
	previous := h.cfg
	next := previous.CloneForRuntime()
	if req.Enabled != nil {
		next.Codex.Runtime.Enabled = *req.Enabled
	}
	if req.Workers != nil {
		oldTokens := make(map[string]string)
		for _, w := range previous.Codex.Runtime.Workers {
			oldTokens[w.ID] = w.Token
		}
		next.Codex.Runtime.Workers = nil
		for _, w := range *req.Workers {
			token := oldTokens[w.ID]
			if w.Token != nil {
				token = *w.Token
			}
			next.Codex.Runtime.Workers = append(next.Codex.Runtime.Workers, config.CodexRuntimeWorker{ID: w.ID, URL: w.URL, Token: token, AuthFile: w.AuthFile, Models: w.Models, Disabled: w.Disabled})
		}
	}
	if err := next.ValidateCodexRuntime(); err != nil {
		h.mu.Unlock()
		c.JSON(400, gin.H{"error": err.Error()})
		return
	}
	h.cfg = next
	snapshot, ok := h.saveConfigAndSnapshotLocked(c)
	if !ok {
		h.cfg = previous
		h.mu.Unlock()
		return
	}
	h.mu.Unlock()
	// Removed workers return their existing file to CPA without deleting credentials.
	for _, w := range previous.Codex.Runtime.Workers {
		if w.AuthFile == "" {
			continue
		}
		found := false
		for _, updated := range next.Codex.Runtime.Workers {
			if updated.ID == w.ID && updated.AuthFile == w.AuthFile {
				found = true
				break
			}
		}
		if !found {
			path := bridge.CredentialPath(previous, w)
			bridge.CancelCredential(path)
			if metadata, err := codexshared.Read(path); err == nil {
				state, shared := codexshared.Get(metadata)
				if shared {
					state.Owner = "cpa"
					codexshared.Set(metadata, state)
					if err := codexshared.Write(path, metadata); err != nil {
						runtimeHTTPError(c, err)
						return
					}
					h.syncRuntimeAuth(c.Request.Context(), path)
				}
			}
		}
	}
	if err := bridge.ReconcileOwners(next); err != nil {
		runtimeHTTPError(c, err)
		return
	}
	for _, w := range next.Codex.Runtime.Workers {
		if w.AuthFile != "" {
			h.syncRuntimeAuth(c.Request.Context(), bridge.CredentialPath(next, w))
		}
	}
	h.reloadConfigAfterManagementSave(c.Request.Context(), snapshot)
	c.JSON(200, runtimeStatus(next))
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

func findRuntimeWorker(cfg *config.Config, id string) (config.CodexRuntimeWorker, bool) {
	for _, w := range cfg.Codex.Runtime.Workers {
		if w.ID == id && w.AuthFile != "" {
			return w, true
		}
	}
	return config.CodexRuntimeWorker{}, false
}

func (h *Handler) setRuntimePreference(ctx context.Context, cfg *config.Config, w config.CodexRuntimeWorker, enabled, create bool) error {
	path := bridge.CredentialPath(cfg, w)
	metadata, err := codexshared.Read(path)
	if err != nil {
		if !create || !os.IsNotExist(err) {
			return err
		}
		metadata = map[string]any{"type": "codex"}
	}
	owner := "cpa"
	if cfg.Codex.Runtime.Enabled && enabled && !w.Disabled {
		owner = "codex"
	}
	bridge.CancelCredential(path)
	codexshared.Set(metadata, codexshared.State{Enabled: enabled, WorkerID: w.ID, Owner: owner})
	if err := codexshared.Write(path, metadata); err != nil {
		return err
	}
	h.syncRuntimeAuth(ctx, path)
	return nil
}

func (h *Handler) SetCodexRuntimeCredential(c *gin.Context) {
	var req struct {
		Name     string `json:"name"`
		WorkerID string `json:"worker_id"`
		Enabled  *bool  `json:"enabled"`
	}
	if c.ShouldBindJSON(&req) != nil || req.Enabled == nil {
		c.JSON(400, gin.H{"error": "name, worker_id and enabled are required"})
		return
	}
	cfg := h.codexRuntimeConfig()
	w, ok := findRuntimeWorker(cfg, req.WorkerID)
	if !ok || req.Name != w.AuthFile {
		c.JSON(400, gin.H{"error": "credential must match the worker auth_file"})
		return
	}
	if err := h.setRuntimePreference(c.Request.Context(), cfg, w, *req.Enabled, false); err != nil {
		runtimeHTTPError(c, err)
		return
	}
	c.JSON(200, runtimeStatus(cfg))
}

func runtimeHTTPError(c *gin.Context, err error) {
	status := http.StatusInternalServerError
	var typed interface{ StatusCode() int }
	if errors.As(err, &typed) {
		status = typed.StatusCode()
	}
	c.JSON(status, gin.H{"error": err.Error()})
}

func (h *Handler) runtimeRPC(c *gin.Context, workerID string, prepareLogin bool) (*bridge.Client, config.CodexRuntimeWorker, bool) {
	cfg := h.codexRuntimeConfig()
	if !cfg.Codex.Runtime.Enabled {
		c.JSON(409, gin.H{"error": "Codex runtime integration is disabled"})
		return nil, config.CodexRuntimeWorker{}, false
	}
	w, ok := findRuntimeWorker(cfg, workerID)
	if !ok || w.Disabled {
		c.JSON(400, gin.H{"error": "Codex runtime worker is unavailable"})
		return nil, w, false
	}
	if prepareLogin {
		if err := h.setRuntimePreference(c.Request.Context(), cfg, w, true, true); err != nil {
			runtimeHTTPError(c, err)
			return nil, w, false
		}
	}
	ctx, release := bridge.Track(c.Request.Context(), bridge.CredentialPath(cfg, w))
	client, err := bridge.Dial(ctx, w.URL, w.Token)
	if err != nil {
		release()
		runtimeHTTPError(c, err)
		return nil, w, false
	}
	client.OnClose(release)
	if client.Caps.CredentialID != w.ID || client.Caps.CredentialFile != filepath.Base(w.AuthFile) {
		client.Close()
		c.JSON(409, gin.H{"error": "Codex runtime worker or credential file mismatch"})
		return nil, w, false
	}
	return client, w, true
}

func (h *Handler) TestCodexRuntime(c *gin.Context) {
	var req struct {
		WorkerID string `json:"worker_id"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "worker_id is required"})
		return
	}
	client, _, ok := h.runtimeRPC(c, req.WorkerID, false)
	if !ok {
		return
	}
	defer client.Close()
	c.JSON(200, gin.H{"status": "ok", "capabilities": client.Caps})
}

func (h *Handler) StartCodexRuntimeLogin(c *gin.Context) {
	var req struct {
		WorkerID string `json:"worker_id"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "worker_id is required"})
		return
	}
	client, _, ok := h.runtimeRPC(c, req.WorkerID, true)
	if !ok {
		return
	}
	defer client.Close()
	var result struct {
		LoginID string `json:"loginId"`
		AuthURL string `json:"authUrl"`
		State   string `json:"state"`
	}
	if err := client.Call("cpa/auth/login/start", map[string]any{}, &result); err != nil {
		runtimeHTTPError(c, err)
		return
	}
	c.JSON(200, gin.H{"login_id": result.LoginID, "url": result.AuthURL, "state": result.State})
}

func (h *Handler) CompleteCodexRuntimeLogin(c *gin.Context) {
	var req struct {
		WorkerID    string `json:"worker_id"`
		LoginID     string `json:"login_id"`
		RedirectURL string `json:"redirect_url"`
	}
	if c.ShouldBindJSON(&req) != nil {
		c.JSON(400, gin.H{"error": "invalid OAuth callback"})
		return
	}
	client, w, ok := h.runtimeRPC(c, req.WorkerID, false)
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
		h.syncRuntimeAuth(c.Request.Context(), bridge.CredentialPath(h.codexRuntimeConfig(), w))
	}
	c.JSON(200, result)
}

func (h *Handler) GetCodexRuntimeLoginStatus(c *gin.Context) {
	client, w, ok := h.runtimeRPC(c, c.Query("worker_id"), false)
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
		h.syncRuntimeAuth(c.Request.Context(), bridge.CredentialPath(h.codexRuntimeConfig(), w))
	}
	c.JSON(200, result)
}
