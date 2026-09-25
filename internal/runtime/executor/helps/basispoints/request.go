package basispoints

import (
	"encoding/json"
	"sort"
	"strconv"
	"strings"
)

// Metadata accepts bounded strings, not arbitrary client JSON objects. Sorting
// makes collisions after key truncation deterministic across request retries.
func requestMetadata(value any) object {
	metadata := object{}
	raw, _ := value.(object)
	keys := make([]string, 0, len(raw))
	for key := range raw {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		var text string
		switch typed := raw[key].(type) {
		case string:
			text = typed
		case json.Number:
			text = typed.String()
		case bool:
			text = strconv.FormatBool(typed)
		default:
			continue
		}
		metadata[truncateRunes(key, 64)] = truncateRunes(text, 512)
	}
	return metadata
}

func truncateRunes(value string, limit int) string {
	chars := []rune(value)
	if len(chars) > limit {
		return string(chars[:limit])
	}
	return value
}

// Tool results use a deterministic function item ID while native calls retain
// the exact server-issued item and call IDs stored in the replay cache.
func functionItemID(callID string) string {
	id := callID
	if !strings.HasPrefix(id, "fc_") {
		id = "fc_" + id
	}
	if len(id) > 64 {
		id = "fc_" + digest(callID)[:61]
	}
	return id
}
