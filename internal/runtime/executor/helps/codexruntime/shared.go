package codexruntime

import (
	"context"
	"io"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
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
	configs  map[string]config.CodexRuntimeConfig
}{requests: make(map[string]map[uint64]context.CancelFunc), enabled: make(map[string]bool), configs: make(map[string]config.CodexRuntimeConfig)}

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

// Credential identifies an existing file by the same relative path as CPA Auth.ID.
type Credential struct {
	ID       string
	Path     string
	Metadata map[string]any
}

func CredentialPath(cfg *config.Config, id string) string {
	if cfg == nil || cfg.AuthDir == "" || id == "" {
		return ""
	}
	return filepath.Join(cfg.AuthDir, filepath.FromSlash(id))
}

func ListCredentials(cfg *config.Config) ([]Credential, error) {
	credentials := make([]Credential, 0)
	if cfg == nil || cfg.AuthDir == "" {
		return credentials, nil
	}
	err := filepath.WalkDir(cfg.AuthDir, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if entry.IsDir() || !strings.EqualFold(filepath.Ext(entry.Name()), ".json") {
			return nil
		}
		metadata, err := codexshared.Read(path)
		if err != nil {
			return nil
		}
		provider, _ := metadata["type"].(string)
		if !strings.EqualFold(strings.TrimSpace(provider), "codex") {
			return nil
		}
		id, err := filepath.Rel(cfg.AuthDir, path)
		if err != nil {
			return err
		}
		if runtime.GOOS == "windows" {
			id = strings.ToLower(id)
		}
		credentials = append(credentials, Credential{ID: id, Path: path, Metadata: metadata})
		return nil
	})
	if os.IsNotExist(err) {
		err = nil
	}
	return credentials, err
}

// LoadSharedAuth rereads shared tokens in either mode without changing the file identity.
func LoadSharedAuth(cfg *config.Config, auth *coreauth.Auth) (*coreauth.Auth, string, error) {
	if auth == nil {
		return auth, "", nil
	}
	_, shared := codexshared.Get(auth.Metadata)
	if !strings.EqualFold(auth.Provider, "codex") {
		return auth, "", nil
	}
	path := auth.Attributes[coreauth.AttributePath]
	if path == "" {
		path = CredentialPath(cfg, auth.ID)
	}
	if path == "" {
		if !shared {
			return auth, "", nil
		}
		return nil, "", fail(400, "Shared Codex credential has no file path")
	}
	metadata, err := codexshared.Read(path)
	if err != nil {
		if !shared && os.IsNotExist(err) {
			return auth, "", nil
		}
		return nil, "", fail(503, "Shared Codex credential could not be read")
	}
	if _, ok := codexshared.Get(metadata); !ok {
		return auth, "", nil
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
	disabled, _ := fresh.Metadata["disabled"].(bool)
	return ctx, fresh, release, Enabled(cfg) && state.Enabled && !disabled, nil
}

type releasedBody struct {
	io.ReadCloser
	release func()
}

func (b *releasedBody) Close() error { defer b.release(); return b.ReadCloser.Close() }
func ReleaseBody(body io.ReadCloser, release func()) io.ReadCloser {
	return &releasedBody{ReadCloser: body, release: release}
}

// ReconcileState updates effective file flags only when selection changes.
func ReconcileState(cfg *config.Config) error {
	_, err := reconcileState(cfg)
	return err
}

func reconcileState(cfg *config.Config, credentialIDs ...string) (bool, error) {
	if cfg == nil || cfg.AuthDir == "" {
		return false, nil
	}
	sharedRequests.Lock()
	previous, known := sharedRequests.configs[cfg.AuthDir]
	current := cfg.Codex.Runtime
	globalChanged := !known || previous.Enabled != current.Enabled
	modeChanged := globalChanged || previous.Endpoint() != current.Endpoint() || !maps.Equal(previous.CredentialModes, current.CredentialModes)
	sharedRequests.enabled[cfg.AuthDir] = current.Enabled
	sharedRequests.Unlock()
	credentials, err := ListCredentials(cfg)
	if err != nil {
		return false, err
	}
	changed := false
	for _, credential := range credentials {
		state, exists := codexshared.Get(credential.Metadata)
		selected, explicit := current.CredentialModes[credential.ID]
		prior, wasExplicit := previous.CredentialModes[credential.ID]
		selectionChanged := globalChanged || prior != selected || wasExplicit != explicit || slices.Contains(credentialIDs, credential.ID)
		// Unrelated reloads and token changes do not override a direct file toggle.
		if !selectionChanged && exists {
			continue
		}
		if !explicit {
			selected = true
			if !known && current.Enabled && exists {
				selected = state.Enabled
			}
		}
		enabled := current.Enabled && selected
		if !exists && !enabled {
			continue
		}
		old, _ := credential.Metadata["codex_cli"].(map[string]any)
		_, oldOwner := old["owner"]
		_, oldWorker := old["worker_id"]
		if exists && state.Enabled == enabled && !oldOwner && !oldWorker {
			continue
		}
		CancelCredential(credential.Path)
		codexshared.Set(credential.Metadata, codexshared.State{Enabled: enabled})
		if err := codexshared.Write(credential.Path, credential.Metadata); err != nil {
			return changed, err
		}
		changed = true
	}
	sharedRequests.Lock()
	current.CredentialModes = maps.Clone(current.CredentialModes)
	sharedRequests.configs[cfg.AuthDir] = current
	sharedRequests.Unlock()
	return changed || modeChanged && (current.Enabled || known && previous.Enabled), nil
}

// ApplyConfig immediately asks the master to apply a changed selection.
func ApplyConfig(ctx context.Context, cfg *config.Config, credentialIDs ...string) error {
	changed, err := reconcileState(cfg, credentialIDs...)
	if err != nil || !changed {
		return err
	}
	client, err := Dial(ctx, cfg.Codex.Runtime.Endpoint())
	if err != nil {
		return err
	}
	defer client.Close()
	return client.Call("cpa/credential/reload", map[string]any{}, nil)
}
