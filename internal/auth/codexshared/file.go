// Package codexshared handles the CPA JSON format used by both credential owners.
package codexshared

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type State struct {
	Enabled bool `json:"enabled"`
}

func Get(metadata map[string]any) (State, bool) {
	v, ok := metadata["codex_cli"].(map[string]any)
	if !ok {
		return State{}, false
	}
	enabled, _ := v["enabled"].(bool)
	return State{Enabled: enabled}, true
}

func Set(metadata map[string]any, state State) {
	v, _ := metadata["codex_cli"].(map[string]any)
	if v == nil {
		v = make(map[string]any)
	}
	delete(v, "worker_id")
	delete(v, "owner")
	v["enabled"] = state.Enabled
	metadata["codex_cli"] = v
}

func Read(path string) (map[string]any, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var metadata map[string]any
	if err := json.Unmarshal(raw, &metadata); err != nil {
		return nil, fmt.Errorf("read Codex credential JSON: %w", err)
	}
	if metadata == nil {
		return nil, fmt.Errorf("Codex credential must be a JSON object")
	}
	return metadata, nil
}

func Write(path string, metadata map[string]any) error {
	raw, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return err
	}
	return os.WriteFile(path, append(raw, '\n'), 0600)
}

var tokenFields = map[string]bool{"access_token": true, "refresh_token": true, "id_token": true, "account_id": true, "email": true, "expired": true, "last_refresh": true}

// SaveTokens updates credential values while retaining the current control flags and metadata.
func SaveTokens(path string, tokens map[string]any) (map[string]any, error) {
	current, err := Read(path)
	if err != nil {
		return nil, err
	}
	for k := range tokenFields {
		if v, ok := tokens[k]; ok {
			current[k] = v
		}
	}
	if err := Write(path, current); err != nil {
		return nil, err
	}
	return current, nil
}

// MergeMetadata keeps token values in the shared file during ordinary CPA status saves.
func MergeMetadata(current, incoming map[string]any) map[string]any {
	for k, v := range incoming {
		if k != "codex_cli" && !tokenFields[k] {
			current[k] = v
		}
	}
	return current
}
