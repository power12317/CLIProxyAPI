package codexruntime

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
)

var sharedRequests = struct {
	sync.Mutex
	next     uint64
	requests map[string]map[uint64]context.CancelFunc
	enabled  map[string]bool
}{requests: make(map[string]map[uint64]context.CancelFunc), enabled: make(map[string]bool)}

func Enabled(cfg *config.Config) bool {
	if cfg == nil {
		return false
	}
	sharedRequests.Lock()
	enabled, ok := sharedRequests.enabled[cfg.AuthDir]
	sharedRequests.Unlock()
	if cfg.AuthDir != "" && ok {
		return enabled
	}
	return cfg.Codex.Runtime.Enabled
}

func SetEnabled(cfg *config.Config) {
	if cfg == nil || cfg.AuthDir == "" {
		return
	}
	sharedRequests.Lock()
	sharedRequests.enabled[cfg.AuthDir] = cfg.Codex.Runtime.Enabled
	sharedRequests.Unlock()
}

func CancelCredential(path string) {
	sharedRequests.Lock()
	for _, cancel := range sharedRequests.requests[path] {
		cancel()
	}
	sharedRequests.Unlock()
}

func Track(ctx context.Context, path string) (context.Context, func()) {
	ctx, cancel := context.WithCancel(ctx)
	sharedRequests.Lock()
	sharedRequests.next++
	id := sharedRequests.next
	if sharedRequests.requests[path] == nil {
		sharedRequests.requests[path] = make(map[uint64]context.CancelFunc)
	}
	sharedRequests.requests[path][id] = cancel
	sharedRequests.Unlock()
	return ctx, func() {
		cancel()
		sharedRequests.Lock()
		delete(sharedRequests.requests[path], id)
		if len(sharedRequests.requests[path]) == 0 {
			delete(sharedRequests.requests, path)
		}
		sharedRequests.Unlock()
	}
}

func CredentialPath(cfg *config.Config, worker config.CodexRuntimeWorker) string {
	return filepath.Join(cfg.AuthDir, worker.AuthFile)
}

func WorkerForAuth(cfg *config.Config, auth *coreauth.Auth) *config.CodexRuntimeWorker {
	if cfg == nil || auth == nil {
		return nil
	}
	name := auth.FileName
	if name == "" {
		name = filepath.Base(auth.Attributes[coreauth.AttributePath])
	}
	state, _ := codexshared.Get(auth.Metadata)
	for i := range cfg.Codex.Runtime.Workers {
		w := &cfg.Codex.Runtime.Workers[i]
		if w.AuthFile != "" && w.AuthFile == name && (state.WorkerID == "" || state.WorkerID == w.ID) {
			return w
		}
	}
	return nil
}

// LoadSharedAuth uses the common credential file on every request, including CPA mode.
func LoadSharedAuth(cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, string, error) {
	if auth == nil {
		return auth, "", nil
	}
	_, shared := codexshared.Get(auth.Metadata)
	w := WorkerForAuth(cfg, auth)
	if !shared && w == nil {
		return auth, "", nil
	}
	path := auth.Attributes[coreauth.AttributePath]
	if w != nil {
		path = CredentialPath(cfg, *w)
	}
	if path == "" {
		return nil, "", fail(400, "Shared Codex credential has no file path")
	}
	metadata, err := codexshared.Read(path)
	if err != nil {
		return nil, "", fail(503, "Shared Codex credential could not be read")
	}
	copy := auth.Clone()
	copy.Metadata = metadata
	return copy, path, nil
}

func PrepareShared(ctx context.Context, cfg *config.Config, auth *coreauth.Auth) (context.Context, *coreauth.Auth, func(), bool, error) {
	fresh, path, err := LoadSharedAuth(cfg, auth)
	if err != nil {
		return ctx, auth, func() {}, false, err
	}
	if path == "" {
		return ctx, auth, func() {}, false, nil
	}
	ctx, release := Track(ctx, path)
	state, _ := codexshared.Get(fresh.Metadata)
	return ctx, fresh, release, Enabled(cfg) && state.Owner == "codex", nil
}

type releasedBody struct {
	io.ReadCloser
	release func()
}

func (b *releasedBody) Close() error { defer b.release(); return b.ReadCloser.Close() }
func ReleaseBody(body io.ReadCloser, release func()) io.ReadCloser {
	return &releasedBody{ReadCloser: body, release: release}
}

// ReconcileOwners records the effective owner without a separate handoff protocol.
func ReconcileOwners(cfg *config.Config) error {
	if cfg == nil {
		return nil
	}
	SetEnabled(cfg)
	entries, err := os.ReadDir(cfg.AuthDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(cfg.AuthDir, entry.Name())
		metadata, err := codexshared.Read(path)
		if err != nil {
			continue
		}
		state, shared := codexshared.Get(metadata)
		if !shared {
			continue
		}
		owner := "cpa"
		if cfg.Codex.Runtime.Enabled && state.Enabled {
			for _, w := range cfg.Codex.Runtime.Workers {
				if w.ID == state.WorkerID && w.AuthFile == entry.Name() && !w.Disabled {
					owner = "codex"
					break
				}
			}
		}
		if state.Owner != owner {
			CancelCredential(path)
			state.Owner = owner
			codexshared.Set(metadata, state)
			if err := codexshared.Write(path, metadata); err != nil {
				return err
			}
		}
	}
	return nil
}
