package helps

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"sort"
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
	codexTurnStateTicketProbeURL       = "https://chatgpt.com/backend-api/codex/responses"
	CodexTurnStateTicketHeader         = "X-Codex-Turn-State"
)

// ErrCodexTurnStateTicketUnavailable is returned when fail-closed is enabled
// but the selected OAuth credential has no valid ticket for the requested model.
var ErrCodexTurnStateTicketUnavailable = errors.New("codex turn-state ticket unavailable")

var codexTurnStateTicketRecorder struct {
	sync.RWMutex
	fn func(*cliproxyauth.Auth, CodexTurnStateTicket)
}

// SetCodexTurnStateTicketRecorder installs the service-owned callback used to
// persist a 780-byte value observed on a normal upstream response.
func SetCodexTurnStateTicketRecorder(fn func(*cliproxyauth.Auth, CodexTurnStateTicket)) {
	codexTurnStateTicketRecorder.Lock()
	codexTurnStateTicketRecorder.fn = fn
	codexTurnStateTicketRecorder.Unlock()
}

func notifyCodexTurnStateTicketRecorder(auth *cliproxyauth.Auth, ticket CodexTurnStateTicket) {
	codexTurnStateTicketRecorder.RLock()
	fn := codexTurnStateTicketRecorder.fn
	codexTurnStateTicketRecorder.RUnlock()
	if fn != nil {
		fn(auth, ticket)
	}
}

// CodexTurnStateTicket is the persisted, credential/model-scoped ticket summary.
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
	TargetLength     int        `json:"target_length"`
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

func (t *CodexTurnStateTicket) valid(now time.Time) bool {
	if t == nil {
		return false
	}
	targetLength := config.DefaultCodexTurnStateTicketTargetLength
	return len(strings.TrimSpace(t.State)) == targetLength && t.Length == targetLength &&
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
	if len(cfg.Models) == 0 {
		cfg.Models = []string{"gpt-6-astra", "gpt-5.6-sol"}
	}
	models := append([]string(nil), cfg.Models...)
	if cfg.CacheAllModelsEnabled() {
		var learned []string
		for key := range auth.Metadata {
			if !IsCodexTurnStateTicketMetadataKey(key) {
				continue
			}
			model := strings.TrimSpace(strings.TrimPrefix(key, CodexTurnStateTicketMetadataPrefix))
			if model != "" && !codexTurnStateTicketModelConfigured(models, model) {
				learned = append(learned, model)
			}
		}
		sort.Strings(learned)
		models = append(models, learned...)
	}
	out := make([]CodexTurnStateTicketStatus, 0, len(models))
	targetLength := config.DefaultCodexTurnStateTicketTargetLength
	for _, model := range models {
		model = strings.TrimSpace(model)
		if model == "" {
			continue
		}
		status := CodexTurnStateTicketStatus{Model: model, TargetLength: targetLength}
		ticket := codexTurnStateTicketForAuth(auth, model)
		if ticket != nil {
			status.Length = ticket.Length
		}
		if ticket.valid(now) {
			status.Ready = true
			status.RemainingSeconds = int64(ticket.ExpiresAt.Sub(now) / time.Second)
			if status.RemainingSeconds < 0 {
				status.RemainingSeconds = 0
			}
			expires := ticket.ExpiresAt
			status.ExpiresAt = &expires
		}
		status.Blocked = cfg.FailClosed && codexTurnStateTicketModelConfigured(cfg.Models, model) && !status.Ready
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
	if !codexTurnStateTicketModelManaged(cfg, model) {
		return nil
	}
	ticket := codexTurnStateTicketForAuth(auth, model)
	if ticket.valid(time.Now()) {
		clearCodexTurnStateHeader(headers)
		headers.Set(CodexTurnStateTicketHeader, ticket.State)
		return nil
	}
	if cfg.FailClosed && codexTurnStateTicketModelConfigured(cfg.Models, model) {
		return fmt.Errorf("%w: model=%s", ErrCodexTurnStateTicketUnavailable, model)
	}
	return nil
}

// ApplyCodexTurnStateTicketBody mirrors a ticket into a websocket request body.
func ApplyCodexTurnStateTicketBody(auth *cliproxyauth.Auth, cfg config.CodexTurnStateTicketConfig, model string, body []byte) []byte {
	if !cfg.Enabled || !isCodexTurnStateTicketAccount(auth) || !codexTurnStateTicketModelManaged(cfg, model) {
		return body
	}
	ticket := codexTurnStateTicketForAuth(auth, model)
	if !ticket.valid(time.Now()) {
		return body
	}
	body, _ = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-state")
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

// RecordCodexTurnStateTicketOnResponse saves a new 780-byte response ticket.
// Missing or other-length values leave the retained ticket and its expiry intact.
func RecordCodexTurnStateTicketOnResponse(auth *cliproxyauth.Auth, cfg config.CodexTurnStateTicketConfig, model string, resp *http.Response) bool {
	if resp == nil || !cfg.Enabled || !isCodexTurnStateTicketAccount(auth) || !codexTurnStateTicketModelManaged(cfg, model) {
		return false
	}
	value := strings.TrimSpace(resp.Header.Get(CodexTurnStateTicketHeader))
	return recordCodexTurnStateTicketValue(auth, cfg, model, value)
}

// RecordCodexTurnStateTicketOnEvent is the WebSocket equivalent. Only
// response metadata headers are inspected; generated text is never treated as
// ticket material.
func RecordCodexTurnStateTicketOnEvent(auth *cliproxyauth.Auth, cfg config.CodexTurnStateTicketConfig, model string, payload []byte) bool {
	if len(payload) == 0 || !cfg.Enabled || !isCodexTurnStateTicketAccount(auth) || !codexTurnStateTicketModelManaged(cfg, model) {
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
	return recordCodexTurnStateTicketValue(auth, cfg, model, value)
}

func recordCodexTurnStateTicketValue(auth *cliproxyauth.Auth, cfg config.CodexTurnStateTicketConfig, model, value string) bool {
	targetLength := config.DefaultCodexTurnStateTicketTargetLength
	if len(value) != targetLength {
		return false
	}
	now := time.Now()
	ticket := CodexTurnStateTicket{
		AccountID:  auth.ID,
		Model:      strings.TrimSpace(model),
		State:      value,
		Length:     len(value),
		CapturedAt: now,
		ExpiresAt:  now.Add(time.Duration(cfg.TTLSeconds) * time.Second),
		Attempts:   1,
	}
	if cfg.TTLSeconds <= 0 {
		ticket.ExpiresAt = now.Add(time.Duration(config.DefaultCodexTurnStateTicketTTLSeconds) * time.Second)
	}
	// Keep the request snapshot unchanged until persistence computes its delta.
	notifyCodexTurnStateTicketRecorder(auth, ticket)
	StoreCodexTurnStateTicket(auth, ticket)
	return true
}

func codexTurnStateTicketModelManaged(cfg config.CodexTurnStateTicketConfig, model string) bool {
	return strings.TrimSpace(model) != "" && (cfg.CacheAllModelsEnabled() || codexTurnStateTicketModelConfigured(cfg.Models, model))
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

// HarvestCodexTurnStateTicket performs one bounded probe using the optional
// harvest proxy. An empty proxy URL uses the normal direct transport; it only
// reads the response header and never uses the response body as ticket data.
func HarvestCodexTurnStateTicket(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, model, harvestProxyURL string) (CodexTurnStateTicket, int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	model = strings.TrimSpace(model)
	harvestProxyURL = strings.TrimSpace(harvestProxyURL)
	if !isCodexTurnStateTicketAccount(auth) {
		return CodexTurnStateTicket{}, 0, nil
	}
	if model == "" {
		return CodexTurnStateTicket{}, 0, errors.New("codex ticket harvest requires a model")
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
	turnMetadataFields := map[string]string{"turn_id": turnID, "session_id": sessionID}
	clientMetadata := map[string]string{"session_id": sessionID}
	if cfg == nil || cfg.Codex.DeviceConvergenceEnabled() {
		installationID := codexInstallationUUID(CodexInstallationAccountID(auth), CodexOAuthClientSystem(auth))
		turnMetadataFields["installation_id"] = installationID
		clientMetadata["x-codex-installation-id"] = installationID
	}
	turnMetadata, _ := json.Marshal(turnMetadataFields)
	clientMetadata["x-codex-turn-metadata"] = string(turnMetadata)
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
		"client_metadata": clientMetadata,
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
	req.Header.Set("X-Codex-Turn-Metadata", string(turnMetadata))
	if accountID, ok := auth.Metadata["account_id"].(string); ok && strings.TrimSpace(accountID) != "" {
		req.Header.Set("Chatgpt-Account-Id", strings.TrimSpace(accountID))
	}
	// Keep the probe headers aligned with the normal Codex OAuth client identity.
	req.Header.Set("Originator", "codex-tui")
	req.Header.Set("Version", CodexClientVersion)
	req.Header.Set("User-Agent", CodexSystemUserAgent(CodexOAuthClientSystem(auth)))
	req.Header.Set("X-Codex-Beta-Features", "remote_compaction_v2")
	req.Header.Set("X-Codex-Routing-Hint", "model="+model)
	req.Header.Set("X-Codex-Window-Id", uuid.NewString())
	req.Header.Set("X-Client-Request-Id", uuid.NewString())
	req.Header.Set("Thread-Id", sessionID)
	client := NewUtlsHTTPClient(attemptCtx, cfg, probeAuth, timeout)
	started := time.Now()
	resp, errDo := client.Do(req)
	logCodexTurnStateTicketProbe(auth, req, model, sessionID, turnID, resp, time.Since(started), errDo)
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
	targetLength := config.DefaultCodexTurnStateTicketTargetLength
	if resp.StatusCode != http.StatusOK || len(state) != targetLength {
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
	updateFn func(context.Context, *cliproxyauth.Auth, *cliproxyauth.Auth) error

	lifecycleMu      sync.Mutex
	cancel           context.CancelFunc
	done             chan struct{}
	configMu         sync.Mutex
	wake             chan struct{}
	enabled          bool
	refreshRequested bool
}

type CodexTurnStateTicketHarvesterOptions struct {
	Config func() *config.Config
	List   func() []*cliproxyauth.Auth
	// Update merges changes from base to updated into the latest credential.
	Update func(ctx context.Context, base, updated *cliproxyauth.Auth) error
}

func NewCodexTurnStateTicketHarvester(opts CodexTurnStateTicketHarvesterOptions) *CodexTurnStateTicketHarvester {
	h := &CodexTurnStateTicketHarvester{
		cfgFn: opts.Config, listFn: opts.List, updateFn: opts.Update,
		wake: make(chan struct{}, 1),
	}
	if cfg := h.currentConfig(); cfg != nil {
		h.enabled = cfg.Codex.EffectiveTurnStateTicket().Enabled
	}
	return h
}

// ConfigChanged immediately wakes the worker when ticket harvesting is enabled.
// Repeated reloads while enabled do not cause extra probes.
func (h *CodexTurnStateTicketHarvester) ConfigChanged() {
	if h == nil {
		return
	}
	h.configMu.Lock()
	defer h.configMu.Unlock()
	cfg := h.currentConfig()
	enabled := cfg != nil && cfg.Codex.EffectiveTurnStateTicket().Enabled
	if enabled && !h.enabled {
		h.refreshRequested = true
		h.signal()
	}
	h.enabled = enabled
}

func (h *CodexTurnStateTicketHarvester) signal() {
	select {
	case h.wake <- struct{}{}:
	default:
	}
}

func (h *CodexTurnStateTicketHarvester) takeRefreshRequest() bool {
	h.configMu.Lock()
	defer h.configMu.Unlock()
	requested := h.refreshRequested
	h.refreshRequested = false
	return requested
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
	SetCodexTurnStateTicketRecorder(nil)
}

func (h *CodexTurnStateTicketHarvester) loop(ctx context.Context) {
	timer := time.NewTimer(0)
	defer timer.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
			h.takeRefreshRequest()
			h.refresh(ctx)
			timer.Reset(h.interval())
		case <-h.wake:
			if h.takeRefreshRequest() {
				h.refresh(ctx)
				timer.Reset(h.interval())
			}
		}
	}
}

func (h *CodexTurnStateTicketHarvester) interval() time.Duration {
	if cfg := h.currentConfig(); cfg != nil {
		return time.Duration(cfg.Codex.EffectiveTurnStateTicket().ProbeIntervalSeconds) * time.Second
	}
	return time.Duration(config.DefaultCodexTurnStateTicketProbeIntervalSeconds) * time.Second
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
	if !policy.Enabled {
		return
	}
	if strings.TrimSpace(policy.HarvestProxyURL) != "" {
		if errProxy := ValidateCodexTurnStateTicketHarvestProxyURL(policy.HarvestProxyURL); errProxy != nil {
			log.WithError(errProxy).Warn("codex turn-state ticket harvester disabled by invalid harvest proxy")
			return
		}
	}
	auths := h.listFn()
	sort.SliceStable(auths, func(i, j int) bool {
		if auths[i] == nil || auths[j] == nil {
			return auths[i] != nil
		}
		return auths[i].ID < auths[j].ID
	})
	for _, auth := range auths {
		if ctx.Err() != nil || !isCodexTurnStateTicketAccount(auth) || auth.Disabled || (auth.Status != "" && auth.Status != cliproxyauth.StatusActive) {
			continue
		}
		for _, model := range policy.Models {
			model = strings.TrimSpace(model)
			if model == "" {
				continue
			}
			// Recheck configuration and credential state after each preceding network request.
			cfg = h.currentConfig()
			if ctx.Err() != nil || cfg == nil || !cfg.Codex.EffectiveTurnStateTicket().Enabled {
				return
			}
			currentPolicy := cfg.Codex.EffectiveTurnStateTicket()
			if !codexTurnStateTicketModelConfigured(currentPolicy.Models, model) {
				continue
			}
			current := h.findAuth(auth.ID)
			if current == nil || !isCodexTurnStateTicketAccount(current) || current.Disabled || (current.Status != "" && current.Status != cliproxyauth.StatusActive) {
				break
			}
			now := time.Now()
			if ticket := codexTurnStateTicketForAuth(current, model); ticket.valid(now) && !ticket.needsRefresh(now, time.Duration(currentPolicy.RefreshBeforeSeconds)*time.Second) {
				continue
			}
			h.probe(ctx, cfg, current, model, currentPolicy.HarvestProxyURL)
		}
	}
}

func (h *CodexTurnStateTicketHarvester) findAuth(authID string) *cliproxyauth.Auth {
	for _, auth := range h.listFn() {
		if auth != nil && auth.ID == authID {
			return auth
		}
	}
	return nil
}

// Record persists a valid ticket observed by a normal upstream request.
func (h *CodexTurnStateTicketHarvester) Record(auth *cliproxyauth.Auth, ticket CodexTurnStateTicket) {
	if h == nil || auth == nil || h.updateFn == nil || strings.TrimSpace(ticket.Model) == "" {
		return
	}
	updated := auth.Clone()
	StoreCodexTurnStateTicket(updated, ticket)
	if errUpdate := h.updateFn(context.Background(), auth, updated); errUpdate != nil {
		log.WithFields(log.Fields{"auth_id": auth.ID, "model": ticket.Model}).Warnf("codex turn-state ticket response persistence failed: %v", errUpdate)
	}
}

func (h *CodexTurnStateTicketHarvester) probe(ctx context.Context, cfg *config.Config, auth *cliproxyauth.Auth, model, proxyURL string) {
	ticket, status, errHarvest := HarvestCodexTurnStateTicket(ctx, cfg, auth, model, proxyURL)
	if errHarvest != nil || status != http.StatusOK || strings.TrimSpace(ticket.State) == "" {
		return
	}
	updated := auth.Clone()
	StoreCodexTurnStateTicket(updated, ticket)
	if errUpdate := h.updateFn(ctx, auth, updated); errUpdate != nil {
		log.WithFields(log.Fields{"auth_id": auth.ID, "model": model}).Warnf("codex turn-state ticket persistence failed: %v", errUpdate)
	}
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
