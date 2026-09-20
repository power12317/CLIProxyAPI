package cliproxy

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func (s *Service) startCodexTicketHarvester(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	harvester := helps.NewCodexTurnStateTicketHarvester(helps.CodexTurnStateTicketHarvesterOptions{
		Config: func() *config.Config {
			s.cfgMu.RLock()
			cfg := s.cfg
			s.cfgMu.RUnlock()
			return cfg
		},
		List: s.coreManager.List,
		Update: func(updateCtx context.Context, auth *coreauth.Auth) error {
			_, errUpdate := s.coreManager.Update(updateCtx, auth)
			return errUpdate
		},
	})
	s.codexTicketHarvester = harvester
	helps.SetCodexTurnStateTicketInvalidator(harvester.Invalidate)
	harvester.Start(ctx)
}

func (s *Service) stopCodexTicketHarvester() {
	if s == nil || s.codexTicketHarvester == nil {
		return
	}
	s.codexTicketHarvester.Stop()
	s.codexTicketHarvester = nil
}
