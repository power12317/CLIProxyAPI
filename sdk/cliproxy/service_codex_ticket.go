package cliproxy

import (
	"context"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

func (s *Service) startCodexTicketHarvester(ctx context.Context) {
	if s == nil || s.coreManager == nil {
		return
	}
	s.codexTicketMu.Lock()
	defer s.codexTicketMu.Unlock()
	if s.codexTicketHarvester != nil {
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
	})
	s.codexTicketHarvester = harvester
	helps.SetCodexTurnStateTicketRecorder(harvester.Record)
	harvester.Start(ctx)
}

func (s *Service) stopCodexTicketHarvester() {
	if s == nil {
		return
	}
	s.codexTicketMu.Lock()
	defer s.codexTicketMu.Unlock()
	if s.codexTicketHarvester == nil {
		return
	}
	s.codexTicketHarvester.Stop()
	s.codexTicketHarvester = nil
	for _, auth := range s.coreManager.List() {
		helps.InvalidateCodexTurnStateTickets(auth.ID)
	}
}

func (s *Service) notifyCodexTicketConfigChanged() {
	if s == nil {
		return
	}
	s.codexTicketMu.Lock()
	defer s.codexTicketMu.Unlock()
	if s.codexTicketHarvester != nil {
		s.codexTicketHarvester.ConfigChanged()
	}
}
