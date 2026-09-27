package cliproxy

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/auth/codexshared"
	internalregistry "github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/watcher/synthesizer"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	"github.com/router-for-me/CLIProxyAPI/v7/sdk/config"
)

func TestCodexRuntimeKeepsFileCredentialAndModelsAcrossModeChanges(t *testing.T) {
	for _, plan := range []string{"free", "plus", "pro"} {
		t.Run(plan, func(t *testing.T) {
			ctx := coreauth.WithSkipPersist(context.Background())
			cfg := &config.Config{}
			manager := coreauth.NewManager(nil, nil, nil)
			service := &Service{cfg: cfg, coreManager: manager}
			dir := t.TempDir()
			name := filepath.Join("team", "原有 "+plan+" account.json")
			path := filepath.Join(dir, name)
			reg := internalregistry.GetGlobalRegistry()
			t.Cleanup(func() { reg.UnregisterClient(name) })
			var initialModels map[string]struct{}
			var initialIndex string
			for _, enabled := range []bool{false, true, false} {
				cfg.Codex.Runtime.Enabled = enabled
				data := []byte(fmt.Sprintf(`{"type":"codex","email":"account@example.com","access_token":"synthetic","refresh_token":"synthetic","account_id":"account","prefix":"existing","plan_type":%q,"codex_cli":{"enabled":%t}}`, plan, enabled))
				auths, err := synthesizer.SynthesizeAuthFile(&synthesizer.SynthesisContext{Config: cfg, AuthDir: dir, Now: time.Unix(1234, 0)}, path, data)
				if err != nil || len(auths) != 1 {
					t.Fatalf("enabled=%t: synthesis error %v, count %d", enabled, err, len(auths))
				}
				service.applyCoreAuthAddOrUpdate(ctx, auths[0])
				auth, ok := manager.GetByID(name)
				if !ok || auth.Provider != "codex" || auth.FileName != filepath.Base(path) || len(manager.List()) != 1 {
					t.Fatalf("enabled=%t: original account registration changed: %+v", enabled, auth)
				}
				registeredExecutor, ok := manager.Executor("codex")
				if _, native := registeredExecutor.(*executor.CodexAutoExecutor); !ok || !native {
					t.Fatalf("enabled=%t: original provider executor missing: %T", enabled, registeredExecutor)
				}
				models := codexModelIDSet(reg.GetModelsForClient(name))
				if len(models) == 0 {
					t.Fatalf("enabled=%t: account has no models", enabled)
				}
				if initialModels == nil {
					initialModels, initialIndex = models, auth.Index
				} else if !reflect.DeepEqual(models, initialModels) || auth.Index != initialIndex {
					t.Fatalf("enabled=%t: model set or account index changed: models=%v index=%q", enabled, models, auth.Index)
				}
			}
		})
	}
}

func TestCodexRuntimeInvalidHotReloadDoesNotCommit(t *testing.T) {
	old := &config.Config{}
	s := &Service{cfg: old}
	cfg := &config.Config{Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{Enabled: true}}}
	cfg.Home.Enabled = true
	if commit := s.commitConfigUpdate(cfg); commit.cfg != nil {
		t.Fatal("unsupported Home/runtime configuration committed")
	}
	if s.cfg != old {
		t.Fatal("active config replaced")
	}
}

func TestCodexRuntimeStopAppliesNativeConfigWhenMasterUnavailable(t *testing.T) {
	var requests atomic.Int32
	master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		http.Error(w, "master unavailable", http.StatusServiceUnavailable)
	}))
	t.Cleanup(master.Close)
	dir := t.TempDir()
	name := "original account.json"
	path := filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(`{"type":"codex","access_token":"synthetic","codex_cli":{"enabled":true}}`), 0600); err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{AuthDir: dir, Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{
		URL:             "ws" + strings.TrimPrefix(master.URL, "http") + "/cpa/v1/ws",
		CredentialModes: map[string]bool{name: true},
	}}}
	clientsUpdated := false
	service := &Service{cfg: &config.Config{}, updateServerClientsContextFn: func(ctx context.Context, applied *config.Config) bool {
		clientsUpdated = applied == cfg
		return clientsUpdated
	}}
	if !service.applyConfigUpdateWithAuthSynthesis(context.Background(), cfg, false) {
		t.Fatal("master communication failure prevented native config application")
	}
	if !clientsUpdated || requests.Load() != 1 {
		t.Fatalf("reload result: clients updated=%t, master calls=%d", clientsUpdated, requests.Load())
	}
	metadata, err := codexshared.Read(path)
	if err != nil {
		t.Fatal(err)
	}
	state, ok := codexshared.Get(metadata)
	if !ok || state.Enabled || metadata["access_token"] != "synthetic" || !cfg.Codex.Runtime.CredentialModes[name] {
		t.Fatal("native mode did not preserve the original credential and preference")
	}
}

type codexRuntimeStartupStore struct {
	list func() ([]*coreauth.Auth, error)
}

func (s codexRuntimeStartupStore) List(context.Context) ([]*coreauth.Auth, error) {
	return s.list()
}
func (codexRuntimeStartupStore) Save(context.Context, *coreauth.Auth) (string, error) {
	return "", nil
}
func (codexRuntimeStartupStore) Delete(context.Context, string) error { return nil }

type codexRuntimeStartupStop struct{ err error }

func (p codexRuntimeStartupStop) Load(context.Context, *config.Config) (*TokenClientResult, error) {
	return nil, p.err
}

func TestCodexRuntimeStartupAppliesFileStateBeforeAuthLoad(t *testing.T) {
	for _, enabled := range []bool{true, false} {
		t.Run(fmt.Sprintf("enabled=%t", enabled), func(t *testing.T) {
			var requests atomic.Int32
			master := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				http.Error(w, "master not started", http.StatusServiceUnavailable)
			}))
			t.Cleanup(master.Close)
			dir := t.TempDir()
			name := "original account.json"
			path := filepath.Join(dir, name)
			initial := `{"type":"codex","access_token":"synthetic"}`
			if !enabled {
				initial = `{"type":"codex","access_token":"synthetic","codex_cli":{"enabled":true}}`
			}
			if err := os.WriteFile(path, []byte(initial), 0600); err != nil {
				t.Fatal(err)
			}
			cfg := &config.Config{AuthDir: dir, Codex: config.CodexConfig{Runtime: config.CodexRuntimeConfig{
				Enabled:         enabled,
				URL:             "ws" + strings.TrimPrefix(master.URL, "http") + "/cpa/v1/ws",
				CredentialModes: map[string]bool{name: true},
			}}}
			loaded := false
			store := codexRuntimeStartupStore{list: func() ([]*coreauth.Auth, error) {
				loaded = true
				metadata, err := codexshared.Read(path)
				if err != nil {
					t.Fatal(err)
				}
				state, ok := codexshared.Get(metadata)
				if !ok || state.Enabled != enabled || metadata["access_token"] != "synthetic" {
					t.Errorf("auth store saw stale startup state: %+v", metadata)
				}
				return nil, nil
			}}
			stop := errors.New("stop after loading auth store")
			service := &Service{cfg: cfg, coreManager: coreauth.NewManager(store, nil, nil), tokenProvider: codexRuntimeStartupStop{err: stop}}
			if err := service.Run(context.Background()); !errors.Is(err, stop) {
				t.Fatalf("startup did not continue after unavailable master: %v", err)
			}
			if !loaded || requests.Load() != 1 {
				t.Fatalf("startup result: auth loaded=%t, master calls=%d", loaded, requests.Load())
			}
		})
	}
}
