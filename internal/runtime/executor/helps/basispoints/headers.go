package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"strings"
)

// SharedCache survives executor replacement during configuration reloads.
var SharedCache Cache

// AccountID reads account metadata only; upstream performs token verification.
func AccountID(token string) string {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return ""
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return ""
	}
	var claims struct {
		Auth struct {
			Account string `json:"chatgpt_account_id"`
		} `json:"https://api.openai.com/auth"`
	}
	if json.Unmarshal(raw, &claims) != nil {
		return ""
	}
	return claims.Auth.Account
}

func Headers(token, account string) http.Header {
	headers := make(http.Header)
	for name, value := range map[string]string{
		"Authorization":           "Bearer " + token,
		"Chatgpt-Account-Id":      account,
		"X-OpenAI-Account-Id":     account,
		"X-Basispoints-Auth-Mode": "chatgpt",
		"Content-Type":            "application/json",
		"Accept":                  "text/event-stream",
		"Origin":                  "https://bps.openai.com",
		"X-OpenAI-Internal-Basispoints-Client-Agent-Profile":  "excel",
		"X-OpenAI-Internal-Basispoints-Client-Editor":         "excel",
		"X-OpenAI-Internal-Basispoints-Client-Host":           "office",
		"X-OpenAI-Internal-Basispoints-Client-Platform":       "excel",
		"X-OpenAI-Internal-Basispoints-Client-Platform-Class": "PC",
		"X-OpenAI-Internal-Basispoints-Client-Product":        "basispoints-excel-plugin",
		"X-OpenAI-Internal-Basispoints-Client-Runtime":        "desktop",
		"X-OpenAI-Internal-Basispoints-Office-Host":           "Excel",
		"X-OpenAI-Internal-Basispoints-Office-Platform":       "PC",
	} {
		headers.Set(name, value)
	}
	return headers
}

// ResponseHeaders removes upstream framing that no longer describes the converted body.
func ResponseHeaders(headers http.Header, stream bool) http.Header {
	out := headers.Clone()
	if out == nil {
		out = make(http.Header)
	}
	for _, key := range []string{"Set-Cookie", "Set-Cookie2", "Content-Length", "Content-Encoding", "Transfer-Encoding"} {
		out.Del(key)
	}
	if stream {
		out.Set("Content-Type", "text/event-stream")
	} else {
		out.Set("Content-Type", "application/json")
	}
	return out
}
