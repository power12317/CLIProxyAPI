package executor

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	coreauth "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/auth"
	coreexecutor "github.com/router-for-me/CLIProxyAPI/v7/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
	"github.com/tidwall/gjson"
)

func TestBasispointsImagesAndMaxThroughHTTP(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprintf("stream=%v", stream), func(t *testing.T) {
			var imageBytes bytes.Buffer
			if err := png.Encode(&imageBytes, image.NewRGBA(image.Rect(0, 0, 2, 3))); err != nil {
				t.Fatal(err)
			}
			dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(imageBytes.Bytes())
			body, _ := json.Marshal(map[string]any{"model": "gpt-6-astra", "reasoning": map[string]any{"effort": "low"}, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_text", "text": "Inspect this image"}, map[string]any{"type": "input_image", "image_url": dataURL, "detail": "high"}}}}})
			var mu sync.Mutex
			uploads, responses := 0, 0
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer image-test-token" {
					t.Error("missing upload/generation auth")
					w.WriteHeader(401)
					return
				}
				account := r.Header.Get("Chatgpt-Account-Id")
				switch r.URL.Path {
				case "/basispoints/api/attachments":
					mu.Lock()
					uploads++
					mu.Unlock()
					reader, err := r.MultipartReader()
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					part, err := reader.NextPart()
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					data, err := io.ReadAll(part)
					if err != nil || part.FormName() != "file" || part.Header.Get("Content-Type") != "image/png" || !bytes.Equal(data, imageBytes.Bytes()) {
						t.Error("actual multipart bytes differ")
						w.WriteHeader(400)
						return
					}
					w.Header().Set("Content-Type", "application/json")
					_, _ = fmt.Fprintf(w, `{"openai_file_id":%q}`, "file-"+account)
				case "/basispoints/api/responses":
					mu.Lock()
					responses++
					mu.Unlock()
					raw, err := io.ReadAll(r.Body)
					if err != nil {
						t.Error(err)
						w.WriteHeader(400)
						return
					}
					if gjson.GetBytes(raw, "reasoning_effort").String() != "xhigh" || gjson.GetBytes(raw, "input.0.type").String() != "configuration_update" || gjson.GetBytes(raw, "input.0.reasoning.effort").String() != "max" {
						t.Error("exact max configuration missing")
						w.WriteHeader(422)
						return
					}
					found := false
					for _, item := range gjson.GetBytes(raw, "input").Array() {
						if item.Get("role").String() != "user" {
							continue
						}
						part := item.Get("content.1")
						found = true
						if part.Get("file_id").String() != "file-"+account || part.Get("image_url").Exists() || part.Get("detail").String() != "high" {
							t.Error("image body would be rejected")
							w.WriteHeader(422)
							return
						}
					}
					if !found {
						t.Error("user image disappeared")
						w.WriteHeader(422)
						return
					}
					w.Header().Set("Content-Type", "text/event-stream")
					_, _ = io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"image-r\",\"status\":\"completed\",\"output\":[]}}\n\n")
				default:
					t.Errorf("unexpected endpoint: %s", r.URL)
					w.WriteHeader(404)
				}
			}))
			defer server.Close()
			target, _ := url.Parse(server.URL)
			ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
				copyReq := r.Clone(r.Context())
				copyURL := *r.URL
				copyURL.Scheme, copyURL.Host = target.Scheme, target.Host
				copyReq.URL = &copyURL
				return server.Client().Transport.RoundTrip(copyReq)
			})))
			exec := NewCodexAutoExecutor(&config.Config{Codex: config.CodexConfig{Basispoints: config.CodexBasispointsConfig{Enabled: true}}})
			for _, account := range []string{"a", "a", "b"} {
				auth := &coreauth.Auth{ID: t.Name() + account, Provider: "codex", Metadata: map[string]any{"access_token": "image-test-token", "account_id": account}}
				req := coreexecutor.Request{Model: "gpt-6-astra(max)", Payload: body}
				opts := coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}
				if stream {
					result, err := exec.ExecuteStream(ctx, auth, req, opts)
					if err != nil {
						t.Fatal(err)
					}
					for chunk := range result.Chunks {
						if chunk.Err != nil {
							t.Fatal(chunk.Err)
						}
					}
				} else {
					if _, err := exec.Execute(ctx, auth, req, opts); err != nil {
						t.Fatal(err)
					}
				}
			}
			mu.Lock()
			defer mu.Unlock()
			if uploads != 2 || responses != 3 {
				t.Fatalf("uploads=%d responses=%d", uploads, responses)
			}
			if !strings.Contains(string(body), dataURL) {
				t.Fatal("source image request was modified")
			}
		})
	}
}

func TestBasispointsDisabledDoesNotUploadImages(t *testing.T) {
	exec := NewCodexAutoExecutor(&config.Config{})
	auth := &coreauth.Auth{ID: t.Name(), Provider: "codex", Metadata: map[string]any{"access_token": "token", "account_id": "account"}}
	calls := 0
	ctx := context.WithValue(t.Context(), "cliproxy.roundtripper", http.RoundTripper(forceWebsocketRoundTripper(func(r *http.Request) (*http.Response, error) {
		calls++
		raw, _ := io.ReadAll(r.Body)
		if r.URL.Host != "chatgpt.com" || !strings.Contains(string(raw), "data:image/png;base64,eA==") || strings.Contains(string(raw), "configuration_update") {
			t.Fatalf("native image path changed: %s %s", r.URL, raw)
		}
		return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("data: {\"type\":\"response.completed\",\"response\":{\"id\":\"native\",\"output\":[]}}\n\n"))}, nil
	})))
	body := []byte(`{"model":"gpt-6-astra","input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,eA=="}]}]}`)
	if _, err := exec.Execute(ctx, auth, coreexecutor.Request{Model: "gpt-6-astra", Payload: body}, coreexecutor.Options{SourceFormat: sdktranslator.FormatOpenAIResponse}); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatal("native request performed an attachment upload")
	}
}
