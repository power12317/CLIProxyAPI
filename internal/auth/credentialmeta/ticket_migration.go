package credentialmeta

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CleanLegacyTurnTicketFiles removes old snapshots once, before workers and watchers start.
// Files without these fields are never rewritten. No token values are logged.
func CleanLegacyTurnTicketFiles(dir string) (int, error) {
	if strings.TrimSpace(dir) == "" {
		return 0, nil
	}
	count := 0
	err := filepath.WalkDir(dir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || !strings.EqualFold(filepath.Ext(path), ".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if _, changed := removeLegacyTurnTicketsJSON(raw); !changed {
			return nil
		}
		changed, err := cleanLegacyTurnTicketFile(path)
		if changed {
			count++
		}
		return err
	})
	return count, err
}

func removeLegacyTurnTicketsJSON(raw []byte) ([]byte, bool) {
	var metadata map[string]json.RawMessage
	if json.Unmarshal(raw, &metadata) != nil {
		return raw, false
	}
	changed := false
	for key := range metadata {
		if strings.HasPrefix(strings.TrimSpace(key), TurnTicketPrefix) {
			delete(metadata, key)
			changed = true
		}
	}
	if !changed {
		return raw, false
	}
	cleaned, err := json.MarshalIndent(metadata, "", "  ")
	if err != nil {
		return raw, false
	}
	return append(cleaned, '\n'), true
}

func cleanLegacyTurnTicketFile(path string) (changed bool, err error) {
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return false, err
	}
	defer func() {
		if errClose := file.Close(); err == nil && errClose != nil {
			err = fmt.Errorf("close cleaned credential: %w", errClose)
		}
	}()
	// Reread through the opened inode so an atomic token refresh cannot be
	// overwritten by renaming an older path snapshot over the refreshed file.
	raw, err := io.ReadAll(file)
	if err != nil {
		return false, err
	}
	cleaned, changed := removeLegacyTurnTicketsJSON(raw)
	if !changed || bytes.Equal(raw, cleaned) {
		return false, nil
	}
	if _, err = file.WriteAt(cleaned, 0); err != nil {
		return false, err
	}
	if err = file.Truncate(int64(len(cleaned))); err != nil {
		return false, err
	}
	return true, nil
}
