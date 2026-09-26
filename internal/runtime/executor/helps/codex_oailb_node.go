package helps

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
)

// CodexOaiLBNode returns only the gateway DNS label, including for an expired
// JWT on an established socket. The cookie is never retained in usage records.
func CodexOaiLBNode(value string) string {
	parts := strings.Split(value, ".")
	if len(parts) != 3 || len(value) > 32768 {
		return ""
	}
	body, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Host string `json:"host"`
	}
	if json.Unmarshal(body, &claims) != nil {
		return ""
	}
	host := strings.TrimSuffix(strings.ToLower(strings.TrimSpace(claims.Host)), ".")
	const prefix, suffix = "chat.gateway.", ".api.openai.com"
	if !strings.HasPrefix(host, prefix) || !strings.HasSuffix(host, suffix) {
		return ""
	}
	node := strings.TrimSuffix(strings.TrimPrefix(host, prefix), suffix)
	if len(node) == 0 || len(node) > 63 || node[0] == '-' || node[len(node)-1] == '-' {
		return ""
	}
	for _, c := range node {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return ""
		}
	}
	return node
}

// CodexOaiLBNodeForExchange gives a response cookie precedence over the actual
// outbound cookie. An explicitly invalid/deleted response cookie is not replaced
// with an invented node or raw host value.
func CodexOaiLBNodeForExchange(request http.Header, response *http.Response) string {
	if response != nil {
		for _, cookie := range response.Cookies() {
			if cookie.Name == "__oailb" {
				return CodexOaiLBNode(cookie.Value)
			}
		}
	}
	for _, cookie := range (&http.Request{Header: request}).Cookies() {
		if cookie.Name == "__oailb" {
			return CodexOaiLBNode(cookie.Value)
		}
	}
	return ""
}

type codexOaiLBReporterKey struct{}

func WithCodexOaiLBReporter(ctx context.Context, reporter *UsageReporter) context.Context {
	return context.WithValue(ctx, codexOaiLBReporterKey{}, reporter)
}

func RecordCodexOaiLBNode(ctx context.Context, node string) {
	if ctx == nil {
		return
	}
	if reporter, _ := ctx.Value(codexOaiLBReporterKey{}).(*UsageReporter); reporter != nil {
		reporter.SetOaiLBNode(node)
	}
	logging.SetCodexOaiLBNode(ginContextFrom(ctx), node)
}

func (r *UsageReporter) SetOaiLBNode(node string) {
	if r == nil {
		return
	}
	r.responseModelMu.Lock()
	r.oaiLBNode = node
	r.responseModelMu.Unlock()
}

func (r *UsageReporter) OaiLBNode() string {
	if r == nil {
		return ""
	}
	r.responseModelMu.RLock()
	defer r.responseModelMu.RUnlock()
	return r.oaiLBNode
}
