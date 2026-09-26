package executor

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/logging"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/runtime/executor/helps/basispoints"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	log "github.com/sirupsen/logrus"
	logtest "github.com/sirupsen/logrus/hooks/test"
)

func TestBasispoints403TemporarilyRoutesToNativeCodex(t *testing.T) {
	const rejection = `{"error":{"message":"403: This request was blocked by our usage policy.","type":"server_error","param":null,"code":null}}`
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
			cooldown := basispoints.NewCooldown(func() time.Time { return now })
			cfg := &config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}}
			exec := NewCodexAutoExecutor(cfg)
			exec.basispointsExec.cooldown = cooldown
			bpsCalls, nativeCalls := 0, 0
			wantBasispoints := true
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
				isBasispoints := r.URL.String() == basispoints.ResponsesURL
				if isBasispoints != wantBasispoints {
					t.Fatalf("unexpected protocol at %s: %s", now, r.URL)
				}
				if isBasispoints {
					bpsCalls++
					if bpsCalls == 1 {
						return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(rejection))}, nil
					}
				} else {
					nativeCalls++
					if r.URL.Host != "chatgpt.com" {
						t.Fatalf("unexpected native destination: %s", r.URL)
					}
				}
				payload := "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_test\",\"status\":\"completed\",\"output\":[]}}\n\n"
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})))
			invoke := func(account string) error {
				auth := &coreauth.Auth{ID: account, Provider: "codex", Metadata: map[string]any{"access_token": "fixture-token", "account_id": account}}
				req := coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":"continue","stream":true}`)}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				if stream {
					result, err := exec.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						return err
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							return chunk.Err
						}
					}
					return nil
				}
				_, err := exec.Execute(ctx, auth, req, opts)
				return err
			}
			err := invoke("account-a")
			var status interface{ StatusCode() int }
			if !errors.As(err, &status) || status.StatusCode() != 403 || err.Error() != rejection || bpsCalls != 1 || nativeCalls != 0 {
				t.Fatalf("initial error changed or replayed: %v calls=%d/%d", err, bpsCalls, nativeCalls)
			}
			if !cfg.Codex.Basispoints.Enabled {
				t.Fatal("cooldown modified the user's config")
			}
			deadline := now.Add(30 * time.Minute)
			if !cooldown.PausedUntil().Equal(deadline) {
				t.Fatal("wrong pause duration")
			}
			wantBasispoints = false
			if err = invoke("account-b"); err != nil {
				t.Fatal(err)
			}
			// Configuration reload replaces executors but retains protocol state.
			reloaded := *cfg
			cfg = &reloaded
			exec = NewCodexAutoExecutor(cfg)
			exec.basispointsExec.cooldown = cooldown
			now = deadline.Add(-time.Nanosecond)
			if err = invoke("account-c"); err != nil {
				t.Fatal(err)
			}
			now = deadline
			wantBasispoints = true
			if err = invoke("account-b"); err != nil {
				t.Fatal(err)
			}
			cooldown.Pause()
			cfg.Codex.Basispoints.Enabled = false
			wantBasispoints = false
			if err = invoke("account-a"); err != nil {
				t.Fatal(err)
			}
			now = now.Add(30 * time.Minute)
			if err = invoke("account-a"); err != nil {
				t.Fatal(err)
			}
			if cfg.Codex.Basispoints.Enabled {
				t.Fatal("automatic recovery overrode a manual disable")
			}
			cfg.Codex.Basispoints.Enabled = true
			wantBasispoints = true
			if err = invoke("account-a"); err != nil {
				t.Fatal(err)
			}
			if bpsCalls != 3 || nativeCalls != 4 {
				t.Fatalf("unexpected route totals: bps=%d native=%d", bpsCalls, nativeCalls)
			}
		})
	}
}

func TestBasispointsCooldownSharedAcrossExecutorReplacement(t *testing.T) {
	first := NewBasispointsExecutor(&config.Config{})
	second := NewCodexAutoExecutor(&config.Config{}).basispointsExec
	if first.cooldown != basispoints.SharedCooldown || second.cooldown != first.cooldown {
		t.Fatal("executor replacement lost the shared protocol state")
	}
}

func TestBasispointsPauseOnlyOn403(t *testing.T) {
	exec := NewBasispointsExecutor(&config.Config{})
	exec.cooldown = basispoints.NewCooldown(nil)
	for _, status := range []int{200, 400, 401, 404, 422, 429, 500, 502} {
		exec.pauseOnForbidden(t.Context(), status)
		if !exec.cooldown.PausedUntil().IsZero() {
			t.Fatalf("HTTP %d paused Basispoints", status)
		}
	}
	exec.pauseOnForbidden(t.Context(), 403)
	if exec.cooldown.PausedUntil().IsZero() {
		t.Fatal("403 did not pause Basispoints")
	}
}

func TestBasispointsAttachment403StartsCooldown(t *testing.T) {
	exec := NewBasispointsExecutor(&config.Config{})
	exec.cooldown = basispoints.NewCooldown(nil)
	requests := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
		requests++
		if r.URL.String() != basispoints.AttachmentsURL {
			t.Fatalf("unexpected upstream after attachment rejection: %s", r.URL)
		}
		return &http.Response{StatusCode: 403, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":"attachment rejected"}`))}, nil
	})))
	auth := &coreauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "fixture-token", "account_id": t.Name()}}
	req := coreexecutor.Request{Model: "gpt-6-astra", Payload: []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,eA=="}]}]}`)}
	_, err := exec.Execute(ctx, auth, req, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse})
	var status interface{ StatusCode() int }
	if !errors.As(err, &status) || status.StatusCode() != 403 || exec.cooldown.PausedUntil().IsZero() || requests != 1 {
		t.Fatalf("attachment rejection did not pause the protocol: %v requests=%d", err, requests)
	}
}

func TestBasispointsPauseDeadlineVisibleInApplicationLog(t *testing.T) {
	logger := log.StandardLogger()
	oldHooks := logger.ReplaceHooks(make(log.LevelHooks))
	oldLevel := logger.GetLevel()
	hook := logtest.NewLocal(logger)
	logger.SetLevel(log.WarnLevel)
	t.Cleanup(func() {
		logger.ReplaceHooks(oldHooks)
		logger.SetLevel(oldLevel)
	})
	now := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	exec := NewBasispointsExecutor(&config.Config{})
	exec.cooldown = basispoints.NewCooldown(func() time.Time { return now })
	exec.pauseOnForbidden(t.Context(), 403)
	exec.pauseOnForbidden(t.Context(), 403)
	entries := hook.AllEntries()
	if len(entries) != 1 {
		t.Fatalf("expected one pause transition, got %d log entries", len(entries))
	}
	formatted, err := (&logging.LogFormatter{}).Format(entries[0])
	if err != nil || !strings.Contains(string(formatted), "paused_until=2026-09-26T12:30:00Z") {
		t.Fatalf("recovery time hidden by application formatter: %s %v", formatted, err)
	}
}
