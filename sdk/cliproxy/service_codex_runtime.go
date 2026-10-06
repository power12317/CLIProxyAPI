package cliproxy

import (
	"context"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
)

func isCodexFileAuth(auth *coreauth.Auth) bool {
	return auth != nil && strings.EqualFold(auth.Provider, "codex") &&
		(auth.FileName != "" || auth.Attributes[coreauth.AttributePath] != "")
}

func (s *Service) scheduleCodexRuntimeReload() {
	if s == nil || s.codexRuntimeReload == nil {
		return
	}
	select {
	case s.codexRuntimeReload <- struct{}{}:
	default:
	}
}

func (s *Service) consumeCodexRuntimeReloads(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case <-s.codexRuntimeReload:
		}
		s.cfgMu.RLock()
		cfg := s.cfg.CloneForRuntime()
		s.cfgMu.RUnlock()
		if cfg == nil || !cfg.Codex.Runtime.Enabled {
			continue
		}
		// A full scan handles both sides of a rename, including the removed ID.
		if err := codexruntime.ReloadCredentials(ctx, cfg); err != nil {
			if ctx.Err() == nil {
				log.WithError(err).Warn("could not reload Codex master after credential file update")
			}
			continue
		}
		log.Debug("reloaded Codex master after credential file update")
	}
}
