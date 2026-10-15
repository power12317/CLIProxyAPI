package helps

import (
	"fmt"
	"io"
	"net/http"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

var codexBorrowPrivateText = regexp.MustCompile(`(?i)bearer\s+[^\s"'<>]+|[^\s"'<>]*\.json|[a-z0-9._%+\-]+@[a-z0-9.\-]+|[a-z0-9_+/=\-]{80,}`)

func codexBorrowErrorSecrets(cfg *config.Config, auth *cliproxyauth.Auth, req *http.Request, resp *http.Response) []string {
	var secrets []string
	if cfg != nil && cfg.CodexHeaderDefaults.OaiLBBorrow != nil {
		c := cfg.CodexHeaderDefaults.OaiLBBorrow
		secrets = append(secrets, c.SourceAuthID, c.SourceAuthFile, c.SourceManagementKey, c.SourceURL)
	}
	if auth != nil {
		secrets = append(secrets, auth.ID, auth.FileName, CodexOAuthAccessToken(auth))
		for _, key := range []string{"refresh_token", "id_token", "email", "account_id"} {
			value, _ := auth.Metadata[key].(string)
			secrets = append(secrets, value)
		}
	}
	if req != nil {
		secrets = append(secrets, req.Header.Get(CodexTurnStateTicketHeader), req.Header.Get("Authorization"), strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer "))
		for _, cookie := range req.Cookies() {
			secrets = append(secrets, cookie.Value)
		}
	}
	if resp != nil {
		secrets = append(secrets, resp.Header.Get(CodexTurnStateTicketHeader))
		for _, cookie := range resp.Cookies() {
			secrets = append(secrets, cookie.Value)
		}
	}
	return secrets
}

func codexBorrowStreamFailure(data []byte, secrets ...string) string {
	if !gjson.ValidBytes(data) {
		return "upstream error body is not JSON"
	}
	// Parse only error fields, never serialize the surrounding response or source bundle.
	redact := func(value string) string { return codexBorrowRedactErrorText(value, secrets) }
	var parts []string
	for _, path := range []string{"type", "response.status", "code", "error.code", "response.error.code", "error.type", "response.error.type"} {
		value := gjson.GetBytes(data, path)
		if value.Type == gjson.String && value.String() != "" {
			parts = append(parts, path+"="+borrowLogText(redact(value.String())))
		}
	}
	for _, path := range []string{"status", "status_code", "error.status", "error.status_code", "response.error.status", "response.error.status_code"} {
		value := gjson.GetBytes(data, path)
		if value.Type == gjson.Number && value.Int() >= 100 && value.Int() <= 599 {
			parts = append(parts, path+"="+strconv.FormatInt(value.Int(), 10))
		}
	}
	for _, path := range []string{"message", "error.message", "response.error.message", "error", "detail"} {
		value := gjson.GetBytes(data, path)
		if value.Type == gjson.String && value.String() != "" {
			parts = append(parts, path+"="+strconv.Quote(redact(value.String())))
		}
	}
	if len(parts) == 0 {
		return "upstream error object has no code or message"
	}
	return strings.Join(parts, ", ")
}

func codexBorrowRedactErrorText(value string, secrets []string) string {
	// Replace longer secrets first, before truncation or escaping can expose fragments.
	ordered := append([]string(nil), secrets...)
	sort.Slice(ordered, func(i, j int) bool { return len(ordered[i]) > len(ordered[j]) })
	for _, secret := range ordered {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
	}
	value = codexBorrowPrivateText.ReplaceAllString(value, "[redacted]")
	const limit = 1024
	runes := []rune(value)
	if len(runes) > limit {
		value = string(runes[:limit]) + "..."
	}
	return value
}

func codexBorrowHTTPFailure(stage string, resp *http.Response, secrets []string, trace *codexBorrowLog) error {
	reason := fmt.Sprintf("%s returned HTTP %d", stage, resp.StatusCode)
	if resp.Body != nil {
		const limit = 16 * 1024
		raw, err := io.ReadAll(io.LimitReader(resp.Body, limit+1))
		switch {
		case err != nil:
			reason += "; error body read failed: " + codexBorrowIOReason(err)
		case len(raw) > limit:
			reason += "; error body exceeds 16384 bytes"
		case len(strings.TrimSpace(string(raw))) != 0:
			trace.observeResponseModel(raw, secrets)
			reason += "; upstream: " + codexBorrowStreamFailure(raw, secrets...)
		}
	}
	return borrowError(reason)
}
