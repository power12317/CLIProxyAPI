package helps

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// The v3 upstream callback already carries the exact exchange headers. Keep
// their pairing local to this inference instead of reading another request's jar.
type codexRuntimeTicketCapture struct {
	auth      *coreauth.Auth
	policy    config.CodexTurnStateTicketConfig
	model     string
	sessionID string
	turnID    string
	request   *http.Request
	response  *http.Response
	route     CodexTicketRoute
	ticket    *CodexTurnStateTicket
	save      func(CodexTurnStateTicket)
	now       func() time.Time
}

func newCodexRuntimeTicketCapture(cfg *config.Config, auth *coreauth.Auth, model string) *codexRuntimeTicketCapture {
	if cfg == nil || !cfg.Codex.EffectiveTurnStateTicket().Enabled || cfg.CodexHeaderDefaults.OaiLBBorrow != nil || !isCodexSourceTicketAccount(auth) || !coreauth.IsCodexRuntimeOwnedAuth(auth) {
		return nil
	}
	return &codexRuntimeTicketCapture{auth: auth, policy: cfg.Codex.EffectiveTurnStateTicket(), model: strings.TrimSpace(model), now: time.Now}
}

func (c *codexRuntimeTicketCapture) Record(entry bridge.UpstreamLog) {
	if c == nil {
		return
	}
	switch entry.Kind {
	case "request":
		c.request, c.response, c.ticket, c.route = nil, nil, nil, CodexTicketRoute{}
		req, err := http.NewRequest(entry.Method, entry.URL, nil)
		if err != nil || entry.Method != http.MethodPost || !strings.HasSuffix(req.URL.Path, "/responses") {
			return
		}
		if model := gjson.Get(entry.Body, "model").String(); model != "" {
			c.model = model
		}
		req.Header = entry.Headers.Clone()
		c.request = req
		identity := codexTurnStateFromRequest(c.auth, entry.URL, []byte(entry.Body), req.Header, false)
		c.sessionID, c.turnID = identity.sessionID, identity.key.turnID
		previous := codexTurnStateTicketForAuth(c.auth, c.model)
		for _, cookie := range req.Cookies() {
			var target, known *CodexRouteCookie
			switch cookie.Name {
			case "__cflb":
				target = &c.route.CFLB
				if previous != nil && previous.Route != nil {
					known = &previous.Route.CFLB
				}
			case "__oailb":
				target = &c.route.OaiLB
				if previous != nil && previous.Route != nil {
					known = &previous.Route.OaiLB
				}
			default:
				continue
			}
			*target = codexRouteCookie(cookie, c.now())
			if known != nil && known.Value == target.Value && !known.ExpiresAt.IsZero() && (target.ExpiresAt.IsZero() || known.ExpiresAt.Before(target.ExpiresAt)) {
				target.ExpiresAt = known.ExpiresAt
			}
		}
	case "response":
		if c.request == nil {
			return
		}
		c.response = &http.Response{StatusCode: entry.StatusCode, Header: entry.Headers.Clone(), Request: c.request}
		c.route, _ = codexResponseRoute(c.route, c.response, c.now(), false)
		c.observe(codexTurnHeaderValue(entry.Headers, CodexTurnStateTicketHeader))
	}
}

func (c *codexRuntimeTicketCapture) Event(event []byte) {
	if c == nil || c.response == nil {
		return
	}
	kind := gjson.GetBytes(event, "type").String()
	if kind != "response.metadata" && kind != "codex.response.metadata" {
		return
	}
	gjson.GetBytes(event, "headers").ForEach(func(key, value gjson.Result) bool {
		if strings.EqualFold(key.String(), CodexTurnStateTicketHeader) && value.Type == gjson.String {
			c.response.Header.Set(CodexTurnStateTicketHeader, value.String())
			c.observe(value.String())
			return false
		}
		return true
	})
}

func (c *codexRuntimeTicketCapture) observe(state string) {
	state = strings.TrimSpace(state)
	now := c.now()
	if !codexTurnStateTicketModelManaged(c.policy, c.model) || len(state) != config.DefaultCodexTurnStateTicketTargetLength || !c.route.valid(now) {
		return
	}
	issued, err := CodexTicketIssuedAt(state, now)
	if err != nil || !now.Before(issued.Add(time.Hour)) {
		return
	}
	// Cookie deadlines were resolved when the response arrived. Reusing that
	// snapshot avoids sliding Max-Age while reading later SSE metadata.
	resp := &http.Response{Request: c.request.WithContext(context.WithValue(c.request.Context(), codexSourceRouteKey{}, c.route))}
	ticket := newCodexSourceTicket(c.auth, c.model, state, resp, now)
	if c.ticket != nil && c.ticket.State == ticket.State && c.ticket.Model == ticket.Model && c.ticket.Route.sameValues(*ticket.Route) {
		return
	}
	c.ticket = &ticket
	if c.save != nil {
		c.save(ticket)
	}
}

// Runtime harvesting uses the existing v3 inference request and the same header
// observer as normal requests. It never enables CPA direct model transport.
func harvestCodexRuntimeTicket(ctx context.Context, cfg *config.Config, auth *coreauth.Auth, model string) (ticket CodexTurnStateTicket, status int, err error) {
	capture := newCodexRuntimeTicketCapture(cfg, auth, model)
	if capture == nil || !bridge.Enabled(cfg) || cfg.Home.Enabled || strings.TrimSpace(model) == "" {
		return ticket, 0, errors.New("codex runtime source harvesting is unavailable")
	}
	ctx, cancel := context.WithTimeout(ctx, time.Duration(capture.policy.AttemptTimeoutSeconds)*time.Second)
	defer cancel()
	ctx, release := bridge.Track(ctx, bridge.CredentialPath(cfg, auth.ID))
	defer release()
	req, sessionID, turnID, err := newCodexTicketProbeRequest(ctx, cfg, auth, model)
	if err != nil {
		return ticket, 0, err
	}
	body, err := io.ReadAll(req.Body)
	_ = req.Body.Close()
	if err != nil {
		return ticket, 0, err
	}
	started := time.Now()
	defer func() {
		if capture.request != nil {
			req = capture.request
			if capture.sessionID != "" {
				sessionID = capture.sessionID
			}
			if capture.turnID != "" {
				turnID = capture.turnID
			}
		}
		logCodexTurnStateTicketProbe(auth, req, model, sessionID, turnID, capture.response, time.Since(started), err)
	}()
	client, err := bridge.Dial(ctx, cfg.Codex.Runtime.Endpoint())
	if err != nil {
		return ticket, 0, err
	}
	defer client.Close()
	client.OnUpstream = capture.Record
	err = client.Start(bridge.Request{RequestID: uuid.NewString(), CredentialID: auth.ID, Operation: "responses", SourceFormat: "codex", SessionID: sessionID, Request: body})
	if capture.response != nil {
		status = capture.response.StatusCode
	}
	if err != nil {
		return ticket, status, err
	}
	reader := bridge.NewSSEReader(&bridge.BodyReader{Client: client})
	for capture.ticket == nil {
		frame, errNext := reader.Next()
		if errNext == io.EOF {
			break
		}
		if errNext != nil {
			return ticket, status, errNext
		}
		capture.Event(frame.Data)
	}
	if capture.response != nil {
		status = capture.response.StatusCode
	}
	if capture.ticket != nil {
		return *capture.ticket, status, nil
	}
	return ticket, status, nil
}
