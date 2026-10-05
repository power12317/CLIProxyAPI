package helps

import (
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/basispoints"
)

// SharedBasispointsWebsocketSessions survives executor replacement on config reload.
var SharedBasispointsWebsocketSessions = NewBasispointsWebsocketSessions()

// BasispointsWebsocketSessions retains transports after requests release them.
// Credential selection happens before lookup; this store never selects an account.
type BasispointsWebsocketSessions struct {
	mu       sync.Mutex
	sessions map[string]*basispoints.WebsocketSession
}

func NewBasispointsWebsocketSessions() *BasispointsWebsocketSessions {
	return &BasispointsWebsocketSessions{sessions: make(map[string]*basispoints.WebsocketSession)}
}

func (s *BasispointsWebsocketSessions) Get(key string) *basispoints.WebsocketSession {
	s.mu.Lock()
	defer s.mu.Unlock()
	if session := s.sessions[key]; session != nil {
		return session
	}
	session := basispoints.NewWebsocketSession(&CodexWebsocketActivity{})
	s.sessions[key] = session
	return session
}

func (s *BasispointsWebsocketSessions) CloseAll() {
	if s == nil {
		return
	}
	s.mu.Lock()
	sessions := s.sessions
	s.sessions = make(map[string]*basispoints.WebsocketSession)
	s.mu.Unlock()
	for _, session := range sessions {
		session.Close()
	}
}
