package basispoints

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
)

// toolObject accepts common transport formatting without evaluating code.
// String repairs run only after strict JSON decoding fails.
func toolObject(value any, stages ...string) (object, error) {
	stage := "tool_arguments"
	if len(stages) > 0 {
		stage = stages[0]
	}
	diagnostic := value
	for depth := 0; depth < 4; depth++ {
		if item, ok := value.(object); ok && item != nil {
			return item, nil
		}
		raw, ok := value.(string)
		if !ok {
			break
		}
		diagnostic = raw
		raw = strings.TrimSpace(raw)
		if decoded, err := decodeToolJSON(raw); err == nil {
			if item, ok := decoded.(object); ok && item != nil {
				return item, nil
			}
			value = decoded
			continue
		}
		if fixed := repairToolJSONStrings(raw); fixed != raw {
			if decoded, err := decodeToolJSON(fixed); err == nil {
				if item, ok := decoded.(object); ok && item != nil {
					return item, nil
				}
				value = decoded
				continue
			}
		}
		if start := strings.Index(raw, "```"); start >= 0 && toolProsePrefix(raw[:start]) {
			fenced := raw[start:]
			newline := strings.IndexByte(fenced, '\n')
			if newline >= 3 && strings.HasSuffix(fenced, "```") {
				language := strings.TrimSpace(fenced[3:newline])
				if language == "" || strings.EqualFold(language, "json") {
					value = strings.TrimSpace(fenced[newline+1 : len(fenced)-3])
					continue
				}
			}
		}
		if start := strings.IndexByte(raw, '{'); start > 0 && toolProsePrefix(raw[:start]) {
			value = raw[start:]
			continue
		}
		break
	}
	return nil, failure(502, "invalid_tool_envelope", fmt.Sprintf("tool transport must contain one JSON object (stage=%s; %s)", stage, toolTransportShape(diagnostic)))
}

func decodeToolJSON(raw string) (any, error) {
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.UseNumber()
	var value, extra any
	if err := decoder.Decode(&value); err != nil {
		return nil, err
	}
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("expected exactly one JSON value")
	}
	return value, nil
}

func toolProsePrefix(prefix string) bool {
	return len(prefix) <= 512 && !strings.ContainsAny(prefix, "{}[]();=`\"")
}

// Preserve existing JSON escapes and exact string contents. Only escape literal
// line breaks/tabs and backslashes that do not start a valid JSON escape.
func repairToolJSONStrings(raw string) string {
	var out strings.Builder
	out.Grow(len(raw))
	quoted := false
	for i := 0; i < len(raw); i++ {
		ch := raw[i]
		if ch == '"' {
			quoted = !quoted
		}
		if quoted {
			switch ch {
			case '\n':
				out.WriteString(`\n`)
				continue
			case '\r':
				out.WriteString(`\r`)
				continue
			case '\t':
				out.WriteString(`\t`)
				continue
			}
		}
		if ch != '\\' || !quoted || i+1 >= len(raw) {
			out.WriteByte(ch)
			continue
		}
		next := raw[i+1]
		valid := strings.ContainsRune(`"\/bfnrt`, rune(next))
		if next == 'u' && i+5 < len(raw) {
			valid = true
			for _, digit := range raw[i+2 : i+6] {
				if !strings.ContainsRune("0123456789abcdefABCDEF", digit) {
					valid = false
					break
				}
			}
		}
		out.WriteByte('\\')
		if valid {
			out.WriteByte(next)
			i++
		} else {
			out.WriteByte('\\')
		}
	}
	return out.String()
}

var toolCallWrapper = regexp.MustCompile(`(?s)^(?:await\s+|return\s+)?([A-Za-z_][A-Za-z0-9_.-]*)\s*\((.*)\)\s*;?\s*$`)

// Recover one complete declared envelope from wrapper text without executing it.
func (b *Bridge) recoverToolEnvelope(value any) (object, bool) {
	raw, ok := value.(string)
	if !ok {
		return nil, false
	}
	raw = strings.TrimSpace(raw)
	if toolCallWrapper.MatchString(raw) {
		return b.recoverExplicitToolEnvelope(raw)
	}
	// A failed JSON document is not wrapper text. Do not turn a complete
	// object nested inside a truncated document into a separate tool call.
	if strings.HasPrefix(raw, "{") || strings.HasPrefix(raw, "[") || strings.HasPrefix(raw, "\"") {
		return nil, false
	}
	if found, ok := b.recoverEmbeddedToolEnvelope(raw); ok {
		return found, true
	}
	if repaired := repairToolJSONStrings(raw); repaired != raw {
		return b.recoverEmbeddedToolEnvelope(repaired)
	}
	return nil, false
}

func (b *Bridge) recoverEmbeddedToolEnvelope(raw string) (object, bool) {
	var found object
	for index := 0; index < len(raw); index++ {
		if raw[index] != '{' {
			continue
		}
		decoder := json.NewDecoder(strings.NewReader(raw[index:]))
		decoder.UseNumber()
		var candidate object
		if decoder.Decode(&candidate) != nil || candidate == nil {
			continue
		}
		index += int(decoder.InputOffset()) - 1
		name, err := envelopeField(candidate, "name", "tool")
		if err != nil {
			return nil, false
		}
		_, known := b.lookupTool(stringValue(name))
		if !known && !nativeName(stringValue(name)) {
			continue
		}
		if candidate["arguments"] == nil && candidate["args"] == nil && candidate["input"] == nil {
			continue
		}
		// Preserve the existing single-envelope contract rather than silently
		// keeping only the first tool from a batch embedded in one code field.
		if found != nil {
			return nil, false
		}
		found = candidate
	}
	return found, found != nil
}

func (b *Bridge) recoverExplicitToolEnvelope(value any) (object, bool) {
	raw, ok := value.(string)
	if !ok {
		return nil, false
	}
	match := toolCallWrapper.FindStringSubmatch(strings.TrimSpace(raw))
	if len(match) != 3 {
		return nil, false
	}
	name := match[1]
	spec, known := b.lookupTool(name)
	if !known && !nativeName(name) {
		return nil, false
	}
	inner, err := toolObject(match[2], "wrapper_arguments")
	if err != nil {
		return nil, false
	}
	if nativeName(name) {
		return object{"name": name, "arguments": inner}, true
	}
	if inner["name"] != nil || inner["tool"] != nil {
		if inner["arguments"] != nil || inner["args"] != nil || inner["input"] != nil {
			candidate, errName := envelopeField(inner, "name", "tool")
			candidateTool, found := b.lookupTool(stringValue(candidate))
			if errName != nil || !found || toolKey(candidateTool) != toolKey(spec) {
				return nil, false
			}
			return inner, true
		}
	}
	if spec.Type != "function" {
		return nil, false
	}
	return object{"name": name, "arguments": inner}, true
}

// Report structure and positions, never source text or JSON parser messages
// that can include bytes from a client's arguments.
func toolTransportShape(value any) string {
	raw, ok := value.(string)
	if !ok {
		if value == nil {
			return "format=missing"
		}
		return fmt.Sprintf("format=non_string_%T", value)
	}
	trimmed := strings.TrimSpace(raw)
	format := "text_or_code"
	switch {
	case trimmed == "":
		format = "empty"
	case strings.HasPrefix(trimmed, "```"):
		format = "markdown"
	case strings.HasPrefix(trimmed, "{"):
		format = "json_object"
	case strings.HasPrefix(trimmed, "["):
		format = "json_array"
	case strings.HasPrefix(trimmed, "\""):
		format = "json_string"
	}
	detail := fmt.Sprintf("format=%s; bytes=%d", format, len(raw))
	var syntax *json.SyntaxError
	var decoded any
	if err := json.Unmarshal([]byte(raw), &decoded); errors.As(err, &syntax) {
		kind := "unexpected_token"
		switch message := syntax.Error(); {
		case strings.Contains(message, "unexpected end of JSON input"):
			kind = "unexpected_eof"
		case strings.Contains(message, "after top-level value"):
			kind = "trailing_data"
		case strings.Contains(message, "in string escape code"), strings.Contains(message, "in \\u hexadecimal character escape"):
			kind = "invalid_escape"
		case syntax.Offset > 0 && syntax.Offset <= int64(len(raw)) && raw[syntax.Offset-1] < 0x20:
			kind = "raw_control"
		case strings.Contains(message, "in string literal"):
			kind = "invalid_string"
		case strings.Contains(message, "after object key"), strings.Contains(message, "after array element"):
			kind = "missing_separator"
		}
		detail += fmt.Sprintf("; json_offset=%d; json_failure=%s", syntax.Offset, kind)
	}
	return detail
}
