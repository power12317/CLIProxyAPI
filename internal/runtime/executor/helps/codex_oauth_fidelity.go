package helps

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strings"

	"github.com/google/uuid"
	"github.com/tidwall/gjson"
)

// CodexOAuthIdentity contains the canonical identifiers shared by the body and
// the protected OAuth request headers.
type CodexOAuthIdentity struct {
	System           string
	InstallationID   string
	SessionID        string
	ThreadID         string
	TurnID           string
	WindowID         string
	ClientRequestID  string
	TurnMetadataJSON string
}

// IsOfficialCodexRequest reports the explicit body marker used by the Codex
// client. A marker is intentionally sufficient; new-api may remove every
// corresponding inbound header before the request reaches CPA.
func IsOfficialCodexRequest(body []byte) bool {
	if len(body) == 0 {
		return false
	}
	value := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata")
	return value.Exists()
}

// ApplyCodexOAuthFidelity reconstructs the official Codex OAuth body. It is
// only called after IsOfficialCodexRequest succeeds.
func ApplyCodexOAuthFidelity(body []byte, accountID string) ([]byte, CodexOAuthIdentity, bool) {
	identity := CodexOAuthIdentity{}
	var document map[string]any
	if errUnmarshal := json.Unmarshal(body, &document); errUnmarshal != nil || document == nil {
		return body, identity, false
	}
	clientMetadata := objectValue(document["client_metadata"])
	if clientMetadata == nil {
		clientMetadata = make(map[string]any)
	}
	turnMetadata := objectValueFromJSON(clientMetadata["x-codex-turn-metadata"])
	if turnMetadata == nil {
		turnMetadata = make(map[string]any)
	}

	identity.System = detectCodexSystem(document, turnMetadata)
	identity.SessionID = firstString(
		stringValue(document["prompt_cache_key"]),
		stringValue(clientMetadata["session_id"]),
		stringValue(turnMetadata["session_id"]),
	)
	if identity.SessionID == "" {
		identity.SessionID = uuid.NewString()
	}
	identity.ThreadID = firstString(stringValue(clientMetadata["thread_id"]), stringValue(turnMetadata["thread_id"]), identity.SessionID)
	identity.TurnID = firstString(stringValue(clientMetadata["turn_id"]), stringValue(turnMetadata["turn_id"]), stringValue(turnMetadata["turn_id"]))
	if identity.TurnID == "" {
		identity.TurnID = uuid.NewString()
	}
	identity.WindowID = firstString(stringValue(clientMetadata["x-codex-window-id"]), stringValue(turnMetadata["window_id"]), identity.SessionID+":0")
	identity.ClientRequestID = identity.SessionID
	identity.InstallationID = codexInstallationUUID(accountID, identity.System)

	clientMetadata["session_id"] = identity.SessionID
	clientMetadata["thread_id"] = identity.ThreadID
	clientMetadata["turn_id"] = identity.TurnID
	clientMetadata["x-codex-window-id"] = identity.WindowID
	clientMetadata["x-codex-installation-id"] = identity.InstallationID
	turnMetadata["session_id"] = identity.SessionID
	turnMetadata["thread_id"] = identity.ThreadID
	turnMetadata["turn_id"] = identity.TurnID
	turnMetadata["window_id"] = identity.WindowID
	turnMetadata["installation_id"] = identity.InstallationID
	rewriteTimezoneMetadata(turnMetadata)
	rewriteTimezoneMetadata(clientMetadata)
	clientMetadata["x-codex-turn-metadata"] = turnMetadata
	document["client_metadata"] = clientMetadata
	document["prompt_cache_key"] = identity.SessionID
	rewriteCodexEnvironmentTimezones(document)

	encoded, errMarshal := json.Marshal(turnMetadata)
	if errMarshal != nil {
		return body, identity, false
	}
	identity.TurnMetadataJSON = string(encoded)
	clientMetadata["x-codex-turn-metadata"] = identity.TurnMetadataJSON
	document["client_metadata"] = clientMetadata
	encodedBody, errMarshalBody := json.Marshal(document)
	if errMarshalBody != nil {
		return body, identity, false
	}
	return encodedBody, identity, true
}

// ApplyCodexInstallationIdentity fixes an installation identity whenever the
// request already carries one, including generic compatibility requests. It
// does not add an installation marker to requests that did not have one.
func ApplyCodexInstallationIdentity(body []byte, accountID string) ([]byte, string, string, bool) {
	var document map[string]any
	if errUnmarshal := json.Unmarshal(body, &document); errUnmarshal != nil || document == nil {
		return body, "", "", false
	}
	clientMetadata := objectValue(document["client_metadata"])
	if clientMetadata == nil {
		return body, "", "", false
	}
	turnMetadata := objectValueFromJSON(clientMetadata["x-codex-turn-metadata"])
	outerInstallation := strings.TrimSpace(stringValue(clientMetadata["x-codex-installation-id"]))
	nestedInstallation := ""
	if turnMetadata != nil {
		nestedInstallation = strings.TrimSpace(stringValue(turnMetadata["installation_id"]))
	}
	if outerInstallation == "" && nestedInstallation == "" {
		return body, "", "", false
	}
	system := detectCodexSystem(document, turnMetadata)
	installationID := codexInstallationUUID(accountID, system)
	clientMetadata["x-codex-installation-id"] = installationID
	turnJSON := ""
	if turnMetadata != nil {
		turnMetadata["installation_id"] = installationID
		encoded, errMarshal := json.Marshal(turnMetadata)
		if errMarshal == nil {
			turnJSON = string(encoded)
			clientMetadata["x-codex-turn-metadata"] = turnJSON
		}
	}
	document["client_metadata"] = clientMetadata
	encodedBody, errMarshalBody := json.Marshal(document)
	if errMarshalBody != nil {
		return body, installationID, turnJSON, false
	}
	return encodedBody, installationID, turnJSON, true
}

// RewriteCodexTurnMetadataInstallation updates an already-forwarded turn
// metadata header when a generic request carries installation identity.
func RewriteCodexTurnMetadataInstallation(raw, installationID string) string {
	if strings.TrimSpace(raw) == "" || strings.TrimSpace(installationID) == "" {
		return raw
	}
	var metadata map[string]any
	if json.Unmarshal([]byte(raw), &metadata) != nil || metadata == nil {
		return raw
	}
	metadata["installation_id"] = installationID
	encoded, errMarshal := json.Marshal(metadata)
	if errMarshal != nil {
		return raw
	}
	return string(encoded)
}

func ApplyCodexOAuthHeaders(headers http.Header, identity CodexOAuthIdentity, model string, stream bool, configuredUserAgent, configuredBeta string) {
	if headers == nil {
		return
	}
	ua := configuredUserAgent
	if strings.TrimSpace(ua) == "" {
		if identity.System == "windows" {
			ua = "codex-tui/0.154.0 (Windows 10.0.19044; x86_64) unknown (codex-tui; 0.154.0)"
		} else {
			ua = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) unknown (codex-tui; 0.154.0)"
		}
	}
	beta := configuredBeta
	if strings.TrimSpace(beta) == "" {
		beta = "remote_compaction_v2"
	}
	headers.Set("Originator", "codex-tui")
	headers.Set("User-Agent", ua)
	headers.Set("Version", "0.154.0")
	headers.Set("X-Codex-Beta-Features", beta)
	headers.Set("X-Codex-Routing-Hint", "model="+strings.TrimSpace(model))
	headers.Set("X-Codex-Window-Id", identity.WindowID)
	headers.Set("X-Codex-Turn-Metadata", identity.TurnMetadataJSON)
	headers.Set("X-Client-Request-Id", identity.ClientRequestID)
	headers.Set("Session-Id", identity.SessionID)
	headers.Set("Thread-Id", identity.ThreadID)
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
}

func codexInstallationUUID(accountID, system string) string {
	sum := md5.Sum([]byte(strings.TrimSpace(accountID) + ":" + strings.ToLower(strings.TrimSpace(system))))
	hexValue := hex.EncodeToString(sum[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:])
}

func detectCodexSystem(document map[string]any, turnMetadata map[string]any) string {
	if turnMetadata != nil {
		if sandbox := strings.TrimSpace(stringValue(turnMetadata["sandbox"])); sandbox == "windows_elevated" || sandbox == "windows_sandbox" {
			return "windows"
		}
	}
	if containsWindowsSafetyRules(document) {
		return "windows"
	}
	if cwd, shell := findEnvironmentContext(document, turnMetadata); windowsDrivePath(cwd) || shell == "cmd" {
		return "windows"
	}
	return "mac"
}

var windowsDrivePathPattern = regexp.MustCompile(`^[A-Za-z]:[\\/]`)

func windowsDrivePath(value string) bool {
	return windowsDrivePathPattern.MatchString(strings.TrimSpace(value))
}

func findEnvironmentContext(document map[string]any, turnMetadata map[string]any) (string, string) {
	var cwd, shell string
	var walk func(any, bool)
	walk = func(value any, inEnvironmentContext bool) {
		switch typed := value.(type) {
		case map[string]any:
			for key, child := range typed {
				normalizedKey := strings.ToLower(strings.ReplaceAll(key, "-", "_"))
				switch normalizedKey {
				case "environment_context":
					walk(child, true)
					continue
				case "environment_context_cwd":
					if cwd == "" {
						cwd = stringValue(child)
					}
				case "environment_context_shell":
					if shell == "" {
						shell = stringValue(child)
					}
				case "cwd":
					if inEnvironmentContext && cwd == "" {
						cwd = stringValue(child)
					}
				case "shell":
					if inEnvironmentContext && shell == "" {
						shell = stringValue(child)
					}
				}
				walk(child, inEnvironmentContext)
			}
		case []any:
			for _, child := range typed {
				walk(child, inEnvironmentContext)
			}
		}
	}
	walk(document, false)
	walk(turnMetadata, false)
	return strings.TrimSpace(cwd), strings.TrimSpace(shell)
}

func containsWindowsSafetyRules(value any) bool {
	found := false
	var walk func(any)
	walk = func(current any) {
		if found {
			return
		}
		switch typed := current.(type) {
		case string:
			return
		case map[string]any:
			for key, child := range typed {
				if strings.EqualFold(key, "description") {
					if description, ok := child.(string); ok && strings.Contains(description, "Windows safety rules:") {
						found = true
						return
					}
				}
				walk(child)
			}
		case []any:
			for _, child := range typed {
				walk(child)
			}
		}
	}
	walk(value)
	return found
}

func rewriteTimezoneMetadata(value map[string]any) {
	for key, child := range value {
		if strings.EqualFold(key, "timezone") {
			if timezone := stringValue(child); isShanghaiOrXinjiangTimezone(timezone) {
				value[key] = "Asia/Singapore"
			}
		}
	}
}

func rewriteCodexEnvironmentTimezones(value any) {
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			if text, ok := child.(string); ok && strings.Contains(text, "<timezone>") && isEnvironmentMetadataText(text) {
				typed[key] = rewriteTimezoneTag(text)
				continue
			}
			rewriteCodexEnvironmentTimezones(child)
		}
	case []any:
		for _, child := range typed {
			rewriteCodexEnvironmentTimezones(child)
		}
	}
}

var timezoneTagPattern = regexp.MustCompile(`(?is)(<timezone>)([^<]*)(</timezone>)`)

func rewriteTimezoneTag(text string) string {
	return timezoneTagPattern.ReplaceAllStringFunc(text, func(tag string) string {
		parts := timezoneTagPattern.FindStringSubmatch(tag)
		if len(parts) != 4 || !isShanghaiOrXinjiangTimezone(strings.TrimSpace(parts[2])) {
			return tag
		}
		return parts[1] + "Asia/Singapore" + parts[3]
	})
}

func isEnvironmentMetadataText(text string) bool {
	lower := strings.ToLower(text)
	return strings.Contains(lower, "environment") || strings.Contains(lower, "sandbox") || strings.Contains(lower, "shell") || strings.Contains(lower, "cwd")
}

func isShanghaiOrXinjiangTimezone(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "asia/shanghai", "asia/urumqi", "asia/kashgar", "asia/chongqing", "prc", "cst-8", "shanghai", "urumqi", "xinjiang", "上海", "新疆":
		return true
	default:
		return false
	}
}

func objectValue(value any) map[string]any {
	if result, ok := value.(map[string]any); ok {
		return result
	}
	return nil
}

func objectValueFromJSON(value any) map[string]any {
	if object := objectValue(value); object != nil {
		return object
	}
	raw, ok := value.(string)
	if !ok || strings.TrimSpace(raw) == "" {
		return nil
	}
	var result map[string]any
	if json.Unmarshal([]byte(raw), &result) != nil {
		return nil
	}
	return result
}

func stringValue(value any) string {
	if value == nil {
		return ""
	}
	if result, ok := value.(string); ok {
		return strings.TrimSpace(result)
	}
	return ""
}

func firstString(values ...string) string {
	for _, value := range values {
		if value = strings.TrimSpace(value); value != "" {
			return value
		}
	}
	return ""
}
