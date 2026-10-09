package codexshared

import "sync"

// runtimeModes shares the applied global mode with refreshers holding older config snapshots.
var runtimeModes sync.Map

func RuntimeEnabled(authDir string, fallback bool) bool {
	if authDir != "" {
		if enabled, ok := runtimeModes.Load(authDir); ok {
			return enabled.(bool)
		}
	}
	return fallback
}

func SetRuntimeEnabled(authDir string, enabled bool) {
	if authDir != "" {
		runtimeModes.Store(authDir, enabled)
	}
}
