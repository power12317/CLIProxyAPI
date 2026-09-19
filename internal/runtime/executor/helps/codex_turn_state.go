package helps

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexTurnStateHeader = "X-Codex-Turn-State"
const codexTurnStateTTL = time.Hour

type codexTurnStateKey struct {
	accountID string
	origin    string
	turnID    string
}

type codexTurnStateEntry struct {
	state     string
	expiresAt time.Time
}

type codexTurnStateBucket struct {
	entries map[codexTurnStateKey]codexTurnStateEntry
}

type codexTurnStateCache struct {
	mu      sync.Mutex
	buckets map[string]*codexTurnStateBucket
	now     func() time.Time
}

var codexTurnStates = newCodexTurnStateCache(time.Now)

func newCodexTurnStateCache(now func() time.Time) *codexTurnStateCache {
	return &codexTurnStateCache{buckets: make(map[string]*codexTurnStateBucket), now: now}
}

// CodexTurnState captures the final identity of one upstream request. A request
// retains its bucket so responses in flight cannot resurrect an invalidated auth.
type CodexTurnState struct {
	cache       *codexTurnStateCache
	bucket      *codexTurnStateBucket
	key         codexTurnStateKey
	authID      string
	authFile    string
	sessionID   string
	override    string
	configured  bool
	requestLen  int
	responseLen int
}

// NewCodexTurnState reads the final outbound headers and uncompressed JSON. It
// does not generate identifiers or change the official-client classification.
func NewCodexTurnState(ctx context.Context, auth *cliproxyauth.Auth, target string, body []byte, headers http.Header, model string, clientHeaders http.Header) *CodexTurnState {
	if ctx == nil {
		ctx = context.Background()
	}
	state := codexTurnStates.request(auth, target, body, headers)
	// Resolve only explicitly configured values. Inbound headers alone do not
	// override state returned by ChatGPT; dynamic configured headers still work.
	if auth != nil {
		attrs := make(map[string]string)
		for key, value := range auth.Attributes {
			if strings.HasPrefix(key, "header:") && strings.EqualFold(strings.TrimSpace(strings.TrimPrefix(key, "header:")), codexTurnStateHeader) {
				attrs[key] = value
			}
		}
		if len(attrs) > 0 {
			req := (&http.Request{Header: make(http.Header)}).WithContext(ctx)
			util.ApplyCustomHeadersFromAttrs(req, attrs, clientHeaders)
			if value := req.Header.Get(codexTurnStateHeader); value != "" {
				state.override, state.configured = value, true
			}
		}
	}
	for key, value := range registry.ModelOverrideHeaders(model) {
		if strings.EqualFold(key, codexTurnStateHeader) && strings.TrimSpace(value) != "" {
			state.override, state.configured = value, true
		}
	}
	return state
}

func (c *codexTurnStateCache) request(auth *cliproxyauth.Auth, target string, body []byte, headers http.Header) *CodexTurnState {
	metadata := gjson.GetBytes(body, "client_metadata")
	turn := metadata.Get("x-codex-turn-metadata")
	if turn.Type == gjson.String {
		turn = gjson.Parse(turn.String())
	}
	headerTurn := gjson.Parse(codexTurnHeaderValue(headers, "X-Codex-Turn-Metadata"))
	state := &CodexTurnState{
		cache: c,
		key: codexTurnStateKey{
			origin: codexTurnOrigin(target),
			turnID: firstString(codexTurnString(headerTurn.Get("turn_id")), codexTurnString(turn.Get("turn_id")), codexTurnString(metadata.Get("turn_id"))),
		},
		sessionID: firstString(codexTurnHeaderValue(headers, "Session-Id"), codexTurnHeaderValue(headers, "Session_id"), codexTurnString(headerTurn.Get("session_id")), codexTurnString(turn.Get("session_id")), codexTurnString(metadata.Get("session_id"))),
	}
	if auth != nil {
		state.authID = auth.ID
		state.authFile = filepath.Base(strings.TrimSpace(auth.FileName))
		if state.authFile == "." || state.authFile == "" {
			state.authFile = filepath.Base(strings.TrimSpace(auth.ID))
		}
		state.key.accountID, _ = auth.Metadata["account_id"].(string)
	}
	if state.authID != "" && state.key.turnID != "" && state.key.origin != "" {
		c.mu.Lock()
		bucket := c.buckets[state.authID]
		if bucket == nil {
			bucket = &codexTurnStateBucket{entries: make(map[codexTurnStateKey]codexTurnStateEntry)}
			c.buckets[state.authID] = bucket
		}
		state.bucket = bucket
		c.mu.Unlock()
	}
	return state
}

func codexTurnString(value gjson.Result) string {
	if value.Type != gjson.String {
		return ""
	}
	return strings.TrimSpace(value.String())
}

func codexTurnHeaderValue(headers http.Header, name string) string {
	if value := headers.Get(name); value != "" {
		return value
	}
	for key, values := range headers {
		if strings.EqualFold(key, name) && len(values) > 0 {
			return values[0]
		}
	}
	return ""
}

func codexTurnOrigin(target string) string {
	u, errParse := url.Parse(target)
	if errParse != nil || u.Host == "" {
		return ""
	}
	scheme := strings.ToLower(u.Scheme)
	switch scheme {
	case "wss":
		scheme = "https"
	case "ws":
		scheme = "http"
	case "http", "https":
	default:
		return ""
	}
	host := strings.ToLower(u.Host)
	if scheme == "https" {
		host = strings.TrimSuffix(host, ":443")
	} else {
		host = strings.TrimSuffix(host, ":80")
	}
	return scheme + "://" + host
}

func (s *CodexTurnState) cached() (string, bool) {
	if s == nil || s.bucket == nil {
		return "", false
	}
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	if s.cache.buckets[s.authID] != s.bucket {
		return "", false
	}
	entry, exists := s.bucket.entries[s.key]
	if !exists {
		return "", false
	}
	if !s.cache.now().Before(entry.expiresAt) {
		delete(s.bucket.entries, s.key)
		return "", false
	}
	return entry.state, true
}

// ApplyHeaders runs after normal header construction. Reading does not renew TTL.
func (s *CodexTurnState) ApplyHeaders(headers http.Header) {
	if s == nil || headers == nil || s.key.turnID == "" {
		return
	}
	if existing := codexTurnHeaderValue(headers, codexTurnStateHeader); existing != "" {
		s.requestLen = len(existing)
	}
	value, found := s.cached()
	if s.configured {
		value, found = s.override, true
	}
	if found {
		for key := range headers {
			if strings.EqualFold(key, codexTurnStateHeader) {
				delete(headers, key)
			}
		}
		headers.Set(codexTurnStateHeader, value)
		s.requestLen = len(value)
	}
}

// ApplyWebsocketBody mirrors state into each frame because a reused websocket
// does not send another HTTP handshake. Missing turn identity remains untouched.
func (s *CodexTurnState) ApplyWebsocketBody(body []byte) []byte {
	if s == nil || s.key.turnID == "" {
		return body
	}
	value, found := s.cached()
	if s.configured {
		value, found = s.override, true
	}
	if !found {
		return body
	}
	s.requestLen = len(value)
	updated, errSet := sjson.SetBytes(body, "client_metadata.x-codex-turn-state", value)
	if errSet != nil {
		return body
	}
	return updated
}

func (s *CodexTurnState) observe(value string) {
	if s == nil {
		return
	}
	s.responseLen = len(value)
	if s.bucket == nil || strings.TrimSpace(value) == "" {
		return
	}
	s.cache.mu.Lock()
	defer s.cache.mu.Unlock()
	if s.cache.buckets[s.authID] == s.bucket {
		s.bucket.entries[s.key] = codexTurnStateEntry{state: value, expiresAt: s.cache.now().Add(codexTurnStateTTL)}
	}
}

// ObserveResponse stores headers immediately, including error responses. A nil
// response or a missing/empty header never removes or renews an existing entry.
func (s *CodexTurnState) ObserveResponse(resp *http.Response) {
	if s == nil {
		return
	}
	if resp == nil {
		s.responseLen = 0
		return
	}
	if resp.Request != nil && resp.Request.URL != nil && codexTurnOrigin(resp.Request.URL.String()) != s.key.origin {
		s.responseLen = 0
		return
	}
	s.observe(codexTurnHeaderValue(resp.Header, codexTurnStateHeader))
}

// ObserveEvent reads only response metadata headers, never generated/tool text.
func (s *CodexTurnState) ObserveEvent(payload []byte) {
	if s == nil {
		return
	}
	kind := gjson.GetBytes(payload, "type").String()
	if kind != "response.metadata" && kind != "codex.response.metadata" {
		return
	}
	gjson.GetBytes(payload, "headers").ForEach(func(key, value gjson.Result) bool {
		if strings.EqualFold(key.String(), codexTurnStateHeader) && value.Type == gjson.String {
			s.observe(value.String())
			return false
		}
		return true
	})
}

// LogResponse attaches the turn-state statistics to the current Gin request.
// GinLogrusLogger writes them on the same access-log line as the HTTP status.
func (s *CodexTurnState) LogResponse(ctx context.Context, cfg *config.Config, websocket bool) {
	if s == nil {
		return
	}
	ginCtx := ginContextFrom(ctx)
	if ginCtx != nil {
		logging.SetCodexTurnStateLogFields(ginCtx, logging.CodexTurnStateLogFields{
			AuthFile:             s.authFile,
			SessionID:            s.sessionID,
			TurnID:               s.key.turnID,
			RequestTurnStateLen:  s.requestLen,
			ResponseTurnStateLen: s.responseLen,
		})
	}
	if !requestLogCaptureEnabled(cfg) {
		return
	}
	if ginCtx == nil {
		return
	}
	fields := []byte(fmt.Sprintf("\nauth_file: %q\nsession_id: %q\nturn_id: %q\nrequest_turn_state_len: %d\nresponse_turn_state_len: %d\n\n", s.authFile, s.sessionID, s.key.turnID, s.requestLen, s.responseLen))
	if websocket {
		appendAPIWebsocketTimeline(ginCtx, fields)
		return
	}
	attempts, attempt := ensureAttempt(ginCtx)
	ensureResponseIntro(ginCtx, attempt)
	writeAttemptResponse(ginCtx, attempt, fields)
	updateAggregatedResponseIfMemoryBacked(ginCtx, attempts)
}

// InvalidateCodexTurnStates drops state for removed or replaced credentials.
func InvalidateCodexTurnStates(authID string) {
	codexTurnStates.invalidate(authID)
}

func (c *codexTurnStateCache) invalidate(authID string) {
	c.mu.Lock()
	delete(c.buckets, authID)
	c.mu.Unlock()
}

func (c *codexTurnStateCache) prune() {
	c.mu.Lock()
	defer c.mu.Unlock()
	now := c.now()
	for _, bucket := range c.buckets {
		for key, entry := range bucket.entries {
			if !now.Before(entry.expiresAt) {
				delete(bucket.entries, key)
			}
		}
	}
}

// StartCodexTurnStateCleanup ties the maintenance loop to the service lifetime.
func StartCodexTurnStateCleanup(ctx context.Context) {
	if ctx == nil {
		ctx = context.Background()
	}
	go func() {
		ticker := time.NewTicker(5 * time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				codexTurnStates.prune()
			}
		}
	}()
}
