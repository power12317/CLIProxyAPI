package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	CodexTurnStateTicketMetadataPrefix = "codex_turn_ticket:"
	codexTurnStateTicketStatePrefix    = "gAAAAA"
	codexTurnStateTicketProbeURL       = "https://chatgpt.com/backend-api/codex/responses"
	CodexTurnStateTicketHeader         = "X-Codex-Turn-State"
)

// ErrCodexTurnStateTicketUnavailable is returned when fail-closed is enabled
// but the selected OAuth credential has no valid ticket for the requested model.
var ErrCodexTurnStateTicketUnavailable = errors.New("codex turn-state ticket unavailable")

var codexTurnStateTicketInvalidator struct {
	sync.RWMutex
	fn func(string, string)
}

// SetCodexTurnStateTicketInvalidator installs the service-owned callback used
// when ChatGPT returns the degraded 312-byte turn state for a proactive ticket.
func SetCodexTurnStateTicketInvalidator(fn func(string, string)) {
	codexTurnStateTicketInvalidator.Lock()
	codexTurnStateTicketInvalidator.fn = fn
	codexTurnStateTicketInvalidator.Unlock()
}

func notifyCodexTurnStateTicketInvalidator(authID, model string) {
	codexTurnStateTicketInvalidator.RLock()
	fn := codexTurnStateTicketInvalidator.fn
	codexTurnStateTicketInvalidator.RUnlock()
	if fn != nil {
		fn(strings.TrimSpace(authID), strings.TrimSpace(model))
	}
}

// CodexTurnStateTicket is the persisted, account/model-scoped ticket summary.
// The state value is intentionally kept out of logs and management responses.
type CodexTurnStateTicket struct {
	AccountID  string    `json:"account_id,omitempty"`
	Model      string    `json:"model"`
	State      string    `json:"state"`
	Length     int       `json:"length"`
	CapturedAt time.Time `json:"captured_at"`
	ExpiresAt  time.Time `json:"expires_at"`
	Attempts   int       `json:"attempts,omitempty"`
}

// CodexTurnStateTicketStatus is safe to expose through the management API.
type CodexTurnStateTicketStatus struct {
	Model            string     `json:"model"`
	Length           int        `json:"length,omitempty"`
	Ready            bool       `json:"ready"`
	RemainingSeconds int64      `json:"remaining_seconds"`
	Blocked          bool       `json:"blocked"`
	ExpiresAt        *time.Time `json:"expires_at,omitempty"`
}

func CodexTurnStateTicketMetadataKey(model string) string {
	return CodexTurnStateTicketMetadataPrefix + strings.TrimSpace(model)
}

func IsCodexTurnStateTicketMetadataKey(key string) bool {
	return strings.HasPrefix(strings.TrimSpace(key), CodexTurnStateTicketMetadataPrefix)
}

func (t *CodexTurnStateTicket) valid(now time.Time, targetLength int) bool {
	if t == nil {
		return false
	}
	if targetLength <= 0 {
		targetLength = config.DefaultCodexTurnStateTicketTargetLength
	}
	return len(strings.TrimSpace(t.State)) == targetLength && t.Length == targetLength &&
		strings.HasPrefix(strings.TrimSpace(t.State), codexTurnStateTicketStatePrefix) &&
		!t.ExpiresAt.IsZero() && now.Before(t.ExpiresAt)
}

func (t *CodexTurnStateTicket) needsRefresh(now time.Time, before time.Duration) bool {
	return t == nil || t.ExpiresAt.IsZero() || !t.ExpiresAt.After(now.Add(before))
}

func codexTurnStateTicketForAuth(auth *cliproxyauth.Auth, model string) *CodexTurnStateTicket {
	if auth == nil || len(auth.Metadata) == 0 || strings.TrimSpace(model) == "" {
		return nil
	}
	raw := auth.Metadata[CodexTurnStateTicketMetadataKey(model)]
	if raw == nil {
		return nil
	}
	b, errMarshal := json.Marshal(raw)
	if errMarshal != nil {
		return nil
	}
	var ticket CodexTurnStateTicket
	if errUnmarshal := json.Unmarshal(b, &ticket); errUnmarshal != nil {
		return nil
	}
	ticket.Model = strings.TrimSpace(model)
	ticket.State = strings.TrimSpace(ticket.State)
	if ticket.Length == 0 {
		ticket.Length = len(ticket.State)
	}
	return &ticket
}

func isCodexTurnStateTicketAccount(auth *cliproxyauth.Auth) bool {
	return auth != nil && strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") && auth.AuthKind() == cliproxyauth.AuthKindOAuth
}

// CodexTurnStateTicketStatuses creates the redacted per-model state shown by
// management clients. It never returns the ticket blob itself.
func CodexTurnStateTicketStatuses(auth *cliproxyauth.Auth, cfg config.CodexTurnStateTicketConfig, now time.Time) []CodexTurnStateTicketStatus {
	if !cfg.Enabled || !isCodexTurnStateTicketAccount(auth) {
		return nil
	}
	if now.IsZero() {
		now = time.Now()
	}
	if cfg.TargetLength <= 0 {
		cfg.TargetLength = config.DefaultCodexTurnStateTicketTargetLength
	}
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"gpt-6-astra", "gpt-5.6-sol"}
	}
	out := make([]CodexTurnStateTicketStatus, 0, len(cfg.Models))
	for _, model := range cfg.Models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		status := CodexTurnStateTicketStatus{Model: model}
		ticket := codexTurnStateTicketForAuth(auth, model)
		if ticket.valid(now, cfg.TargetLength) {
			status.Ready = true
			status.Length = ticket.Length
			status.RemainingSeconds = int64(ticket.ExpiresAt.Sub(now) / time.Second)
			if status.RemainingSeconds < 0 {
				status.RemainingSeconds = 0
			}
			expires := ticket.ExpiresAt
			status.ExpiresAt = &expires
		}
		status.Blocked = cfg.FailClosed && !status.Ready
		out = append(out, status)
	}
	return out
}

// ApplyCodexTurnStateTicket applies a valid proactive ticket after the normal
// Codex headers have been assembled. It never replaces a ticket with an
// inbound client value and returns an error only for a configured fail-closed
// model.
func ApplyCodexTurnStateTicket(auth *cliproxyauth.Auth, cfg config.CodexTurnStateTicketConfig, model string, headers http.Header) error {
	if headers == nil || !cfg.Enabled || !isCodexTurnStateTicketAccount(auth) {
		return nil
	}
	model = strings.TrimSpace(model)
	if model == "" || !codexTurnStateTicketModelConfigured(cfg.Models, model) {
		return nil
	}
	clearCodexTurnStateHeader(headers)
	ticket := codexTurnStateTicketForAuth(auth, model)
	if ticket.valid(time.Now(), cfg.TargetLength) {
		headers.Set(CodexTurnStateTicketHeader, ticket.State)
		return nil
	}
	if cfg.FailClosed {
		return fmt.Errorf("%w: model=%s", ErrCodexTurnStateTicketUnavailable, model)
	}
	return nil
}

// ApplyCodexTurnStateTicketBody mirrors a ticket into a websocket request body.
func ApplyCodexTurnStateTicketBody(auth *cliproxyauth.Auth, cfg config.CodexTurnStateTicketConfig, model string, body []byte) []byte {
	if !cfg.Enabled || !isCodexTurnStateTicketAccount(auth) || !codexTurnStateTicketModelConfigured(cfg.Models, strings.TrimSpace(model)) {
		return body
	}
	body, _ = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-state")
	ticket := codexTurnStateTicketForAuth(auth, model)
	if !ticket.valid(time.Now(), cfg.TargetLength) {
		return body
	}
	updated, errSet := sjson.SetBytes(body, "client_metadata.x-codex-turn-state", ticket.State)
	if errSet != nil {
		return body
	}
	return updated
}

func clearCodexTurnStateHeader(headers http.Header) {
	for key := range headers {
		if strings.EqualFold(key, CodexTurnStateTicketHeader) {
			delete(headers, key)
		}
	}
}

// InvalidateCodexTurnStateTicketOnResponse invalidates a still-valid 292 ticket
// when the upstream returns the known degraded 312-byte state. The harvester
// callback removes the persisted value and starts a fresh probe immediately.
func InvalidateCodexTurnStateTicketOnResponse(auth *cliproxyauth.Auth, cfg config.CodexTurnStateTicketConfig, model string, resp *http.Response) bool {
	if resp == nil || !cfg.Enabled || !isCodexTurnStateTicketAccount(auth) || !codexTurnStateTicketModelConfigured(cfg.Models, model) {
		return false
	}
	value := strings.TrimSpace(resp.Header.Get(CodexTurnStateTicketHeader))
	return invalidateCodexTurnStateTicketOnValue(auth, cfg, model, value)
}

// InvalidateCodexTurnStateTicketOnEvent is the WebSocket equivalent. Only
// response metadata headers are inspected; generated text is never treated as
// ticket material.
func InvalidateCodexTurnStateTicketOnEvent(auth *cliproxyauth.Auth, cfg config.CodexTurnStateTicketConfig, model string, payload []byte) bool {
	if len(payload) == 0 || !cfg.Enabled || !isCodexTurnStateTicketAccount(auth) || !codexTurnStateTicketModelConfigured(cfg.Models, model) {
		return false
	}
	kind := gjson.GetBytes(payload, "type").String()
	if kind != "response.metadata" && kind != "codex.response.metadata" {
		return false
	}
	value := ""
	gjson.GetBytes(payload, "headers").ForEach(func(key, item gjson.Result) bool {
		if strings.EqualFold(key.String(), CodexTurnStateTicketHeader) && item.Type == gjson.String {
			value = strings.TrimSpace(item.String())
			return false
		}
		return true
	})
	return invalidateCodexTurnStateTicketOnValue(auth, cfg, model, value)
}

func invalidateCodexTurnStateTicketOnValue(auth *cliproxyauth.Auth, cfg config.CodexTurnStateTicketConfig, model, value string) bool {
	if len(value) != 312 {
		return false
	}
	ticket := codexTurnStateTicketForAuth(auth, model)
	if !ticket.valid(time.Now(), cfg.TargetLength) {
		return false
	}
	notifyCodexTurnStateTicketInvalidator(auth.ID, model)
	return true
}

func codexTurnStateTicketModelConfigured(models []string, model string) bool {
	model = strings.TrimSpace(model)
	for _, configured := range models {
		if strings.TrimSpace(configured) == model {
			return true
		}
	}
	return false
}

// StoreCodexTurnStateTicket updates the in-memory auth and is intentionally
// separate from persistence so callers can use Manager.Update atomically.
func StoreCodexTurnStateTicket(auth *cliproxyauth.Auth, ticket CodexTurnStateTicket) {
	if auth == nil || strings.TrimSpace(ticket.Model) == "" || strings.TrimSpace(ticket.State) == "" {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	ticket.AccountID = auth.ID
	ticket.Model = strings.TrimSpace(ticket.Model)
	ticket.State = strings.TrimSpace(ticket.State)
	ticket.Length = len(ticket.State)
	auth.Metadata[CodexTurnStateTicketMetadataKey(ticket.Model)] = ticket
}

// HarvestCodexTurnStateTicket performs one bounded probe using the dedicated
// harvest proxy. It only reads the response header; the response body is never
// used as a source of ticket material.
func HarvestCodexTurnStateTicket(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, model, harvestProxyURL string) (CodexTurnStateTicket, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	model = strings.TrimSpace(model)
	harvestProxyURL = strings.TrimSpace(harvestProxyURL)
	if !isCodexTurnStateTicketAccount(auth) {
		return CodexTurnStateTicket{}, 0, nil
	}
	if model == "" || harvestProxyURL == "" {
		return CodexTurnStateTicket{}, 0, errors.New("codex ticket harvest requires a model and harvest proxy")
	}
	token, _ := auth.Metadata["access_token"].(string)
	token = strings.TrimSpace(token)
	if token == "" {
		return CodexTurnStateTicket{}, 0, errors.New("codex OAuth access token is unavailable")
	}
	var effective config.CodexTurnStateTicketConfig
	if cfg != nil {
		effective = cfg.Codex.EffectiveTurnStateTicket()
	} else {
		var codexCfg config.CodexConfig
		effective = codexCfg.EffectiveTurnStateTicket()
	}
	timeout := time.Duration(effective.AttemptTimeoutSeconds) * time.Second
	attemptCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	probeAuth := auth.Clone()
	probeAuth.ProxyURL = harvestProxyURL
	sessionID := uuid.NewString()
	turnID := uuid.NewString()
	turnMetadata, _ := json.Marshal(map[string]string{"turn_id": turnID, "session_id": sessionID})
	probeDocument := map[string]any{
		"model":        model,
		"store":        false,
		"stream":       true,
		"instructions": "Reply with exactly: pong",
		"input": []map[string]any{{
			"role": "user",
			"content": []map[string]string{{
				"type": "input_text",
				"text": "ping",
			}},
		}},
		"client_metadata": map[string]string{
			"session_id":            sessionID,
			"x-codex-turn-metadata": string(turnMetadata),
		},
	}
	body, errMarshalBody := json.Marshal(probeDocument)
	if errMarshalBody != nil {
		return CodexTurnStateTicket{}, 0, errMarshalBody
	}
	req, errRequest := http.NewRequestWithContext(attemptCtx, http.MethodPost, codexTurnStateTicketProbeURL, bytes.NewReader(body))
	if errRequest != nil {
		return CodexTurnStateTicket{}, 0, errRequest
	}
	req.Host = "chatgpt.com"
	req.Close = true
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("OpenAI-Beta", "responses=experimental")
	req.Header.Set("Session-Id", sessionID)
	if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
		req.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))
	}
	// The probe must look like a recent official Codex client for Astra tickets.
	// These headers match the normal OAuth fidelity path, including the identity
	// values that ChatGPT uses when deciding whether to issue a 292 state.
	req.Header.Set("Originator", "codex-tui")
	req.Header.Set("Version", "0.154.0")
	req.Header.Set("User-Agent", "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) unknown (codex-tui; 0.154.0)")
	req.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	req.Header.Set("X-Codex-Routing-Hint", "model="+model)
	req.Header.Set("X-Codex-Window-Id", uuid.NewString())
	req.Header.Set("X-Client-Request-Id", uuid.NewString())
	req.Header.Set("Thread-Id", sessionID)
	client := NewUtlsHTTPClient(attemptCtx, cfg, probeAuth, timeout)
	resp, errDo := client.Do(req)
	if errDo != nil {
		return CodexTurnStateTicket{}, 0, errDo
	}
	if resp == nil {
		return CodexTurnStateTicket{}, 0, errors.New("codex ticket probe returned no response")
	}
	defer func() {
		if resp.Body != nil {
			_ = resp.Body.Close()
		}
	}()
	state := strings.TrimSpace(resp.Header.Get(CodexTurnStateTicketHeader))
	targetLength := effective.TargetLength
	if resp.StatusCode != http.StatusOK || len(state) != targetLength || !strings.HasPrefix(state, codexTurnStateTicketStatePrefix) {
		return CodexTurnStateTicket{}, resp.StatusCode, nil
	}
	now := time.Now()
	return CodexTurnStateTicket{AccountID: auth.ID, Model: model, State: state, Length: len(state), CapturedAt: now, ExpiresAt: now.Add(time.Duration(effective.TTLSeconds) * time.Second), Attempts: 1}, resp.StatusCode, nil
}

// CodexTurnStateTicketHarvester continuously refreshes tickets in the
// background. Callbacks keep this helper independent of the auth manager so it
// can be used by both the standalone service and the embeddable SDK.
type CodexTurnStateTicketHarvester struct {
	cfgFn    func() *config.Config
	listFn   func() []*cliproxyauth.Auth
	updateFn func(context.Context, *cliproxyauth.Auth) error

	lifecycleMu sync.Mutex
	cancel      context.CancelFunc
	done        chan struct{}
	flightMu    sync.Mutex
	flights     map[string]struct{}
}

type CodexTurnStateTicketHarvesterOptions struct {
	Config func() *config.Config
	List   func() []*cliproxyauth.Auth
	Update func(context.Context, *cliproxyauth.Auth) error
}

func NewCodexTurnStateTicketHarvester(opts CodexTurnStateTicketHarvesterOptions) *CodexTurnStateTicketHarvester {
	return &CodexTurnStateTicketHarvester{cfgFn: opts.Config, listFn: opts.List, updateFn: opts.Update, flights: make(map[string]struct{})}
}

// Invalidate removes the current ticket for an account/model pair and starts a
// replacement probe without waiting for the next periodic sweep.
func (h *CodexTurnStateTicketHarvester) Invalidate(authID, model string) {
	if h == nil || strings.TrimSpace(authID) == "" || strings.TrimSpace(model) == "" || h.listFn == nil || h.updateFn == nil {
		return
	}
	cfg := h.currentConfig()
	if cfg == nil {
		return
	}
	policy := cfg.Codex.EffectiveTurnStateTicket()
	if !policy.Enabled || !codexTurnStateTicketModelConfigured(policy.Models, model) || strings.TrimSpace(policy.HarvestProxyURL) == "" {
		return
	}
	for _, auth := range h.listFn() {
		if auth == nil || strings.TrimSpace(auth.ID) != strings.TrimSpace(authID) || !isCodexTurnStateTicketAccount(auth) {
			continue
		}
		updated := auth.Clone()
		if updated.Metadata != nil {
			delete(updated.Metadata, CodexTurnStateTicketMetadataKey(model))
		}
		if errUpdate := h.updateFn(context.Background(), updated); errUpdate != nil {
			log.WithFields(log.Fields{"auth_id": authID, "model": model}).Warnf("codex turn-state ticket invalidation persistence failed: %v", errUpdate)
		}
		// The persisted ticket is invalidated before the request returns; only
		// the replacement network probe runs asynchronously.
		h.probe(context.Background(), cfg, updated, strings.TrimSpace(model), policy.HarvestProxyURL)
		return
	}
}

func (h *CodexTurnStateTicketHarvester) Start(parent context.Context) {
	if h == nil {
		return
	}
	if parent == nil {
		parent = context.Background()
	}
	h.lifecycleMu.Lock()
	if h.done != nil {
		h.lifecycleMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(parent)
	h.cancel = cancel
	h.done = make(chan struct{})
	done := h.done
	h.lifecycleMu.Unlock()
	go func() {
		defer close(done)
		h.loop(ctx)
	}()
}

func (h *CodexTurnStateTicketHarvester) Stop() {
	if h == nil {
		return
	}
	h.lifecycleMu.Lock()
	cancel, done := h.cancel, h.done
	h.cancel, h.done = nil, nil
	h.lifecycleMu.Unlock()
	if cancel != nil {
		cancel()
	}
	if done != nil {
		<-done
	}
	SetCodexTurnStateTicketInvalidator(nil)
}

func (h *CodexTurnStateTicketHarvester) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			h.refresh(ctx)
			interval := config.DefaultCodexTurnStateTicketProbeIntervalSeconds
			if cfg := h.currentConfig(); cfg != nil {
				interval = cfg.Codex.EffectiveTurnStateTicket().ProbeIntervalSeconds
			}
			if interval <= 0 {
				interval = config.DefaultCodexTurnStateTicketProbeIntervalSeconds
			}
			timer.Reset(time.Duration(interval) * time.Second)
		}
	}
}

func (h *CodexTurnStateTicketHarvester) currentConfig() *config.Config {
	if h == nil || h.cfgFn == nil {
		return nil
	}
	return h.cfgFn()
}

func (h *CodexTurnStateTicketHarvester) refresh(ctx context.Context) {
	if h == nil || h.listFn == nil || h.updateFn == nil || ctx.Err() != nil {
		return
	}
	cfg := h.currentConfig()
	if cfg == nil {
		return
	}
	policy := cfg.Codex.EffectiveTurnStateTicket()
	if !policy.Enabled || strings.TrimSpace(policy.HarvestProxyURL) == "" {
		return
	}
	if errProxy := ValidateCodexTurnStateTicketHarvestProxyURL(policy.HarvestProxyURL); errProxy != nil {
		log.WithError(errProxy).Warn("codex turn-state ticket harvester disabled by invalid harvest proxy")
		return
	}
	now := time.Now()
	refreshBefore := time.Duration(policy.RefreshBeforeSeconds) * time.Second
	for _, auth := range h.listFn() {
		if ctx.Err() != nil || !isCodexTurnStateTicketAccount(auth) || auth.Disabled || (auth.Status != "" && auth.Status != cliproxyauth.StatusActive) {
			continue
		}
		for _, model := range policy.Models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			if ticket := codexTurnStateTicketForAuth(auth, model); ticket.valid(now, policy.TargetLength) && !ticket.needsRefresh(now, refreshBefore) {
				continue
			}
			h.probe(ctx, cfg, auth, model, policy.HarvestProxyURL)
		}
	}
}

func (h *CodexTurnStateTicketHarvester) probe(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, model, proxyURL string) {
	key := strings.TrimSpace(auth.ID) + "\x00" + strings.TrimSpace(model)
	h.flightMu.Lock()
	if _, exists := h.flights[key]; exists {
		h.flightMu.Unlock()
		return
	}
	h.flights[key] = struct{}{}
	h.flightMu.Unlock()
	go func() {
		defer func() {
			h.flightMu.Lock()
			delete(h.flights, key)
			h.flightMu.Unlock()
		}()
		ticket, status, errHarvest := HarvestCodexTurnStateTicket(ctx, cfg, auth, model, proxyURL)
		if errHarvest != nil {
			log.WithFields(log.Fields{"auth_id": auth.ID, "model": model}).Debugf("codex turn-state ticket probe failed: %v", errHarvest)
			return
		}
		if status != http.StatusOK || strings.TrimSpace(ticket.State) == "" {
			log.WithFields(log.Fields{"auth_id": auth.ID, "model": model, "http_status": status}).Debug("codex turn-state ticket probe did not return a ticket")
			return
		}
		updated := auth.Clone()
		StoreCodexTurnStateTicket(updated, ticket)
		if errUpdate := h.updateFn(ctx, updated); errUpdate != nil {
			log.WithFields(log.Fields{"auth_id": auth.ID, "model": model}).Warnf("codex turn-state ticket persistence failed: %v", errUpdate)
			return
		}
		log.WithFields(log.Fields{"auth_id": auth.ID, "model": model, "length": ticket.Length}).Info("codex turn-state ticket harvested")
	}()
}

// ValidateCodexTurnStateTicketHarvestProxyURL validates syntax without making
// a network request or including credentials in an error message.
func ValidateCodexTurnStateTicketHarvestProxyURL(raw string) error {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed.Hostname() == "" || parsed.Opaque != "" || parsed.RawQuery != "" || parsed.ForceQuery || parsed.Fragment != "" || (parsed.Path != "" && parsed.Path != "/") {
		return errors.New("harvest proxy must be an HTTP(S) or SOCKS5(h) URL with a host and no path, query or fragment")
	}
	switch strings.ToLower(parsed.Scheme) {
	case "http", "https", "socks5", "socks5h":
	default:
		return errors.New("harvest proxy scheme must be http, https, socks5 or socks5h")
	}
	return nil
}

// MaskCodexTurnStateTicketHarvestProxyURL preserves the proxy host while
// ensuring management responses never return a stored proxy password.
func MaskCodexTurnStateTicketHarvestProxyURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" || ValidateCodexTurnStateTicketHarvestProxyURL(raw) != nil {
		return ""
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed.User == nil {
		return raw
	}
	if _, hasPassword := parsed.User.Password(); hasPassword {
		parsed.User = url.UserPassword(parsed.User.Username(), "***")
	}
	return parsed.String()
}

func IsMaskedCodexTurnStateTicketHarvestProxyURL(raw string) bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return true
	}
	parsed, errParse := url.Parse(raw)
	if errParse != nil || parsed.User == nil {
		return false
	}
	password, ok := parsed.User.Password()
	return ok && password == "***"
}

// RedactCodexTurnStateTicketMetadata removes server-managed ticket material
// from a management export while retaining unrelated auth metadata.
func RedactCodexTurnStateTicketMetadata(metadata map[string]any) map[string]any {
	if metadata == nil {
		return nil
	}
	redacted := make(map[string]any, len(metadata))
	for key, value := range metadata {
		if !IsCodexTurnStateTicketMetadataKey(key) {
			redacted[key] = value
		}
	}
	return redacted
}
