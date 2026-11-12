package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
)

// ResolveBasispointsCredentials reads and refreshes only the Office partition.
// Shared file locks serialize OAuth rotation with account edits and binding.
// No refreshed token is returned before its rotated grant is durable.
func ResolveBasispointsCredentials(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, path string) (token, account string, err error) {
	if auth == nil {
		return "", "", fmt.Errorf("Basispoints requires a separate Office authorization")
	}
	service := codex.NewCodexAuthWithProxyURL(cfg, auth.ProxyURL)
	return resolveBasispointsCredentials(ctx, auth, path, service.RefreshBasispointsTokens)
}

func resolveBasispointsCredentials(ctx context.Context, auth *coreauth.Auth, path string, refresh func(context.Context, map[string]any) (*codex.BasispointsTokenData, error)) (token, account string, err error) {
	resolve := func() error {
		var metadata map[string]any
		var before []byte
		if path != "" {
			var errRead error
			before, errRead = os.ReadFile(path)
			decoder := json.NewDecoder(bytes.NewReader(before))
			decoder.UseNumber()
			if errRead != nil || decoder.Decode(&metadata) != nil {
				return fmt.Errorf("Basispoints credential file is unavailable")
			}
		} else {
			metadata = auth.Metadata
		}
		grant, _ := metadata["basispoints"].(map[string]any)
		if grant["error"] == "reauthorization_required" {
			return fmt.Errorf("Basispoints Office authorization must be renewed")
		}
		if errIdentity := codex.ValidateBasispointsIdentity(grant, metadata); errIdentity != nil {
			return errIdentity
		}
		selectedAccount, _, expiry, errValidate := codex.ValidateBasispointsGrant(grant, time.Now(), true)
		if errValidate != nil {
			return errValidate
		}
		if outer, _ := metadata["account_id"].(string); outer != "" && outer != selectedAccount {
			return fmt.Errorf("Basispoints Office workspace does not match this account")
		}
		if expiry.Before(time.Now().Add(5*time.Minute)) || grant["_access_token_rejected"] == true {
			if path == "" {
				return fmt.Errorf("Basispoints Office grant must be refreshed in its owning credential store")
			}
			refreshed, errRefresh := refresh(ctx, grant)
			if errRefresh != nil {
				var tokenError *codex.BasispointsTokenError
				if errors.As(errRefresh, &tokenError) && tokenError.ReauthorizationRequired {
					latest, errRead := os.ReadFile(path)
					if errRead != nil || string(latest) != string(before) {
						return fmt.Errorf("Basispoints credential changed while refreshing; retry the request")
					}
					grant["error"] = "reauthorization_required"
					if errSave := codex.WriteJSONFileAtomic(path, metadata); errSave != nil {
						return fmt.Errorf("failed to persist Office authorization status")
					}
				}
				return errRefresh
			}
			// Re-read to reject external file replacement while acquiring credentials.
			latest, errRead := os.ReadFile(path)
			if errRead != nil || string(latest) != string(before) {
				return fmt.Errorf("Basispoints credential changed while refreshing; retry the request")
			}
			metadata["basispoints"] = refreshed.ToMap()
			if errSave := codex.WriteJSONFileAtomic(path, metadata); errSave != nil {
				return fmt.Errorf("failed to persist rotated Basispoints Office grant")
			}
			grant = refreshed.ToMap()
		}
		token, _ = grant["access_token"].(string)
		account = selectedAccount
		return nil
	}
	if path != "" {
		err = codex.WithFileLock(path, resolve)
	} else {
		err = resolve()
	}
	return
}

// RejectBasispointsAccessToken never invalidates a concurrently rotated grant.
func RejectBasispointsAccessToken(path, token string) error {
	if path == "" || token == "" {
		return nil
	}
	return codex.WithFileLock(path, func() error {
		raw, errRead := os.ReadFile(path)
		if errRead != nil {
			return errRead
		}
		var metadata map[string]any
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		if decoder.Decode(&metadata) != nil {
			return fmt.Errorf("invalid Office credential file")
		}
		grant, _ := metadata["basispoints"].(map[string]any)
		if grant["access_token"] != token || grant["_access_token_rejected"] == true {
			return nil
		}
		grant["_access_token_rejected"] = true
		return codex.WriteJSONFileAtomic(path, metadata)
	})
}
