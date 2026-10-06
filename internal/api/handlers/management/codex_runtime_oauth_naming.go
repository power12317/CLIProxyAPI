package management

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/gin-gonic/gin"
	codexauth "github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// Callback completion and status polling may observe the same completed login.
var runtimeOAuthCompletionMu sync.Mutex

// runtimeOAuthRecord uses the same identity, plan and system naming as native CPA OAuth.
func runtimeOAuthRecord(metadata map[string]any) (*coreauth.Auth, error) {
	raw, err := json.Marshal(metadata)
	if err != nil {
		return nil, fmt.Errorf("encode Codex credential: %w", err)
	}
	storage := &codexauth.CodexTokenStorage{}
	if err := json.Unmarshal(raw, storage); err != nil {
		return nil, fmt.Errorf("read Codex credential tokens: %w", err)
	}
	storage.Email = strings.TrimSpace(storage.Email)
	storage.PlanType = strings.TrimSpace(storage.PlanType)
	storage.AccountID = strings.TrimSpace(storage.AccountID)
	if claims, errParse := codexauth.ParseJWTToken(storage.IDToken); errParse == nil && claims != nil {
		if storage.Email == "" {
			storage.Email = strings.TrimSpace(claims.GetUserEmail())
		}
		if accountID := strings.TrimSpace(claims.GetAccountID()); accountID != "" {
			storage.AccountID = accountID
		}
		if plan := strings.TrimSpace(claims.CodexAuthInfo.ChatgptPlanType); plan != "" {
			storage.PlanType = plan
		}
	}
	if storage.Email == "" || strings.TrimSpace(storage.AccessToken) == "" {
		return nil, fmt.Errorf("completed Codex OAuth credential is missing email or access token")
	}
	if storage.PlanType == "" {
		storage.PlanType = codexauth.DefaultPlanType
	}
	hashAccountID := ""
	if storage.AccountID != "" {
		digest := sha256.Sum256([]byte(storage.AccountID))
		hashAccountID = hex.EncodeToString(digest[:])[:8]
	}
	system, _ := metadata["codex_client_system"].(string)
	name := codexCredentialFileNameForSystem(codexauth.CredentialFileName(storage.Email, storage.PlanType, hashAccountID, true), system)
	if filepath.Base(name) != name || strings.ContainsAny(name, "/\\") {
		return nil, fmt.Errorf("Codex OAuth identity cannot be used as a credential filename")
	}
	metadata["email"] = storage.Email
	metadata["account_id"] = storage.AccountID
	metadata["plan_type"] = storage.PlanType
	metadata["auth_kind"] = coreauth.AuthKindOAuth
	return &coreauth.Auth{
		ID: name, FileName: name, Provider: "codex", Storage: storage,
		Metadata: metadata, Attributes: map[string]string{"plan_type": storage.PlanType},
	}, nil
}

func (h *Handler) completeRuntimeOAuth(c *gin.Context, state string, metadata map[string]any) bool {
	runtimeOAuthCompletionMu.Lock()
	defer runtimeOAuthCompletionMu.Unlock()
	_, _, _, _, completed, exists := GetOAuthSessionDetails(state)
	if exists && completed {
		return true
	}
	if err := guardOAuthSessionPendingForSave(state, "codex"); err != nil {
		runtimeHTTPError(c, err)
		return false
	}
	ctx := c.Request.Context()
	credentialID, _ := metadata["credential_id"].(string)
	cfg := h.codexRuntimeConfig()
	path := bridge.CredentialPath(cfg, credentialID)
	if isNew, _ := metadata["new_credential"].(bool); isNew {
		credential, err := codexshared.Read(path)
		if err != nil {
			runtimeHTTPError(c, fmt.Errorf("read completed Codex OAuth credential: %w", err))
			return false
		}
		record, err := runtimeOAuthRecord(credential)
		if err != nil {
			runtimeHTTPError(c, err)
			return false
		}
		// Use CPA's normal save path so repeat logins preserve existing account settings.
		savedPath, err := h.saveTokenRecord(ctx, record)
		if err != nil {
			runtimeHTTPError(c, fmt.Errorf("save named Codex OAuth credential: %w", err))
			return false
		}
		bridge.CancelCredential(path)
		// Stop the old worker before removal so a refresh cannot recreate its UUID file.
		placeholder := maps.Clone(credential)
		placeholder["codex_cli"] = map[string]any{"enabled": false}
		if err := codexshared.Write(path, placeholder); err != nil {
			runtimeHTTPError(c, fmt.Errorf("disable Codex OAuth placeholder: %w", err))
			return false
		}
		client, err := bridge.Dial(ctx, cfg.Codex.Runtime.Endpoint())
		if err != nil {
			runtimeHTTPError(c, err)
			return false
		}
		err = client.Call("cpa/credential/reload", map[string]any{"credentialId": credentialID}, nil)
		client.Close()
		if err != nil {
			runtimeHTTPError(c, fmt.Errorf("stop Codex OAuth placeholder worker: %w", err))
			return false
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			runtimeHTTPError(c, fmt.Errorf("remove Codex OAuth placeholder: %w", err))
			return false
		}
		h.removeAuthsForPath(ctx, path, credentialID)
		_, ok := h.saveRuntimeConfig(c, func(next *config.Config) error {
			selected, found := next.Codex.Runtime.CredentialModes[credentialID]
			if !found {
				selected = true
			}
			if next.Codex.Runtime.CredentialModes == nil {
				next.Codex.Runtime.CredentialModes = make(map[string]bool)
			}
			delete(next.Codex.Runtime.CredentialModes, credentialID)
			next.Codex.Runtime.CredentialModes[record.ID] = selected
			return nil
		}, record.ID)
		if !ok {
			return false
		}
		path = savedPath
	}
	h.syncRuntimeAuth(ctx, path)
	CompleteOAuthSession(state)
	return true
}
