package helps

import (
	"crypto/md5"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/tidwall/gjson"
)

// CodexOAuthIdentity contains the canonical identifiers shared by the body and
// the protected OAuth request headers.
type CodexOAuthIdentity struct {
	System             string
	InstallationID     string
	SessionID          string
	PromptCacheKey     string
	ResponsesSessionID string
	ThreadID           string
	TurnID             string
	WindowID           string
	ClientRequestID    string
	TurnMetadataJSON   string
}

// CodexInstallationAccountID returns the shared identity seed for normal
// requests and ticket probes, falling back to the credential ID when needed.
func CodexInstallationAccountID(auth *cliproxyauth.Auth) string {
	if accountID := CodexOAuthAccountID(auth); accountID != "" {
		return accountID
	}
	if auth != nil {
		return strings.TrimSpace(auth.ID)
	}
	return ""
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
func ApplyCodexOAuthFidelity(body []byte, accountID, credentialSystem string, deviceConvergence bool) ([]byte, CodexOAuthIdentity, bool) {
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

	identity.System = normalizeCodexSystem(credentialSystem)
	if strings.TrimSpace(credentialSystem) == "" {
		identity.System = detectCodexSystem(document, turnMetadata)
	}
	identity.SessionID = firstString(
		stringValue(turnMetadata["session_id"]),
		stringValue(clientMetadata["session_id"]),
		stringValue(turnMetadata["thread_id"]),
		stringValue(clientMetadata["thread_id"]),
		stringValue(document["prompt_cache_key"]),
	)
	if identity.SessionID == "" {
		identity.SessionID = uuid.NewString()
	}
	identity.ThreadID = firstString(stringValue(clientMetadata["thread_id"]), stringValue(turnMetadata["thread_id"]), identity.SessionID)
	identity.PromptCacheKey = firstString(stringValue(document["prompt_cache_key"]), identity.SessionID)
	identity.ResponsesSessionID = identity.PromptCacheKey
	if stringValue(turnMetadata["subagent_kind"]) != "" || stringValue(clientMetadata["x-openai-subagent"]) != "" {
		identity.ResponsesSessionID = identity.SessionID
	}
	identity.TurnID = firstString(stringValue(clientMetadata["turn_id"]), stringValue(turnMetadata["turn_id"]))
	// Native prewarm runs before a turn exists. Do not invent a routing identity.
	prewarm := document["generate"] == false && stringValue(turnMetadata["request_kind"]) == "prewarm"
	if identity.TurnID == "" && !prewarm {
		identity.TurnID = uuid.NewString()
	}
	windowNumber := "0"
	if number, errParse := strconv.ParseUint(fmt.Sprint(turnMetadata["window_number"]), 10, 64); errParse == nil {
		windowNumber = strconv.FormatUint(number, 10)
	}
	identity.WindowID = firstString(stringValue(clientMetadata["x-codex-window-id"]), stringValue(turnMetadata["window_id"]), identity.ThreadID+":"+windowNumber)
	identity.ClientRequestID = identity.ThreadID
	if deviceConvergence {
		identity.InstallationID = codexInstallationUUID(accountID, identity.System)
	} else {
		identity.InstallationID = firstString(
			stringValue(clientMetadata["x-codex-installation-id"]),
			stringValue(turnMetadata["installation_id"]),
		)
	}

	clientMetadata["session_id"] = identity.SessionID
	clientMetadata["thread_id"] = identity.ThreadID
	clientMetadata["turn_id"] = identity.TurnID
	clientMetadata["x-codex-window-id"] = identity.WindowID
	if deviceConvergence {
		clientMetadata["x-codex-installation-id"] = identity.InstallationID
	}
	turnMetadata["session_id"] = identity.SessionID
	turnMetadata["thread_id"] = identity.ThreadID
	turnMetadata["turn_id"] = identity.TurnID
	turnMetadata["window_id"] = identity.WindowID
	if deviceConvergence {
		turnMetadata["installation_id"] = identity.InstallationID
	}
	rewriteTimezoneMetadata(turnMetadata)
	rewriteTimezoneMetadata(clientMetadata)
	clientMetadata["x-codex-turn-metadata"] = turnMetadata
	document["client_metadata"] = clientMetadata
	document["prompt_cache_key"] = identity.PromptCacheKey
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
func ApplyCodexInstallationIdentity(body []byte, accountID, credentialSystem string, deviceConvergence bool) ([]byte, string, string, bool) {
	if !deviceConvergence {
		return body, "", "", false
	}
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
	system := normalizeCodexSystem(credentialSystem)
	if strings.TrimSpace(credentialSystem) == "" {
		system = detectCodexSystem(document, turnMetadata)
	}
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

func ApplyCodexOAuthHeaders(headers http.Header, identity CodexOAuthIdentity, stream bool, configuredUserAgent, configuredBeta string) {
	applyCodexOAuthIdentityHeaders(headers, identity, configuredUserAgent, configuredBeta)
	if headers == nil {
		return
	}
	if stream {
		headers.Set("Accept", "text/event-stream")
	} else {
		headers.Set("Accept", "application/json")
	}
}

// ApplyCodexOAuthWebsocketHeaders applies identity without HTTP response negotiation.
// Explicit Accept overrides already present in the header map are preserved.
func ApplyCodexOAuthWebsocketHeaders(headers http.Header, identity CodexOAuthIdentity, configuredUserAgent, configuredBeta string) {
	applyCodexOAuthIdentityHeaders(headers, identity, configuredUserAgent, configuredBeta)
}

func applyCodexOAuthIdentityHeaders(headers http.Header, identity CodexOAuthIdentity, configuredUserAgent, configuredBeta string) {
	if headers == nil {
		return
	}
	ua := configuredUserAgent
	if strings.TrimSpace(ua) == "" {
		ua = CodexSystemUserAgent(identity.System)
		if clientUA := headers.Get("User-Agent"); IsCodexClientUserAgent(clientUA) {
			ua = clientUA
		}
	}
	beta := configuredBeta
	if strings.TrimSpace(beta) == "" {
		beta = MergeCodexBetaFeatures(headers.Get("X-Codex-Beta-Features"), "remote_compaction_v2")
	}
	if headers.Get("Originator") == "" {
		headers.Set("Originator", "codex-tui")
	}
	headers.Set("User-Agent", ua)
	if headers.Get("Version") == "" {
		headers.Set("Version", CodexClientVersion)
	}
	headers.Set("X-Codex-Beta-Features", beta)
	if headers.Get("X-Codex-Window-Id") == "" {
		headers.Set("X-Codex-Window-Id", identity.WindowID)
	}
	turnMetadata := headers.Get("X-Codex-Turn-Metadata")
	if turnMetadata == "" {
		turnMetadata = identity.TurnMetadataJSON
	} else if identity.InstallationID != "" {
		turnMetadata = RewriteCodexTurnMetadataInstallation(turnMetadata, identity.InstallationID)
	}
	headers.Set("X-Codex-Turn-Metadata", CodexTurnMetadataHeader(turnMetadata))
	headers.Set("X-Client-Request-Id", identity.ClientRequestID)
	headers.Set("Session-Id", firstString(identity.ResponsesSessionID, identity.SessionID))
	headers.Set("Thread-Id", identity.ThreadID)
}

// ApplyCodexOAuthRoutingHint derives routing from the final uncompressed body.
// Callers apply explicit credential and model header overrides afterward.
func ApplyCodexOAuthRoutingHint(headers http.Header, body []byte) {
	if headers == nil {
		return
	}
	model := strings.TrimSpace(gjson.GetBytes(body, "model").String())
	if model == "" {
		return
	}
	hint := "model=" + model
	if tier := gjson.GetBytes(body, "service_tier"); tier.Type == gjson.String && tier.String() != "" {
		hint += ";tier=" + tier.String()
	}
	headers.Set("X-Codex-Routing-Hint", hint)
}

func codexInstallationUUID(accountID, system string) string {
	sum := md5.Sum([]byte(strings.TrimSpace(accountID) + ":" + strings.ToLower(strings.TrimSpace(system))))
	hexValue := hex.EncodeToString(sum[:])
	return fmt.Sprintf("%s-%s-%s-%s-%s", hexValue[:8], hexValue[8:12], hexValue[12:16], hexValue[16:20], hexValue[20:])
}

func normalizeCodexSystem(system string) string {
	if strings.EqualFold(strings.TrimSpace(system), "windows") {
		return "windows"
	}
	return "mac"
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

// MergeCodexBetaFeatures preserves client features while adding required defaults once.
func MergeCodexBetaFeatures(values ...string) string {
	seen := make(map[string]bool)
	var features []string
	for _, value := range values {
		for _, feature := range strings.Split(value, ",") {
			feature = strings.TrimSpace(feature)
			if feature != "" && !seen[feature] {
				seen[feature] = true
				features = append(features, feature)
			}
		}
	}
	return strings.Join(features, ",")
}
