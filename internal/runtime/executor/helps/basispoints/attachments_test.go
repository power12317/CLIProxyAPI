package basispoints

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/tidwall/gjson"
)

type attachmentTransport func(*http.Request) (*http.Response, error)

func (f attachmentTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestInlineImagesUploadAndAccountRotation(t *testing.T) {
	image := []byte("exact image upload bytes")
	dataURL := "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)
	body := object{"model": "gpt-6-astra", "reasoning_effort": "xhigh", "metadata": object{"turn_id": "same-turn", "number": json.Number("9007199254740993")}, "input": []any{
		object{"type": "configuration_update", "reasoning": object{"effort": "max"}},
		object{"role": "user", "content": []any{object{"type": "input_text", "text": dataURL}, object{"type": "input_image", "image_url": dataURL, "detail": "high"}, object{"type": "input_image", "image_url": "https://cdn.example/img.png", "detail": "low"}, object{"type": "input_image", "file_id": "existing-file"}}},
		object{"type": "function_call_output", "call_id": "c1", "output": []any{object{"type": "input_image", "image_url": dataURL}}},
		object{"type": "custom_tool_call_output", "call_id": "c2", "output": []any{object{"type": "input_image", "image_url": dataURL, "detail": "original"}}},
	}}
	raw, _ := json.Marshal(body)
	cache := &AttachmentCache{}
	uploads := 0
	client := &http.Client{Transport: attachmentTransport(func(r *http.Request) (*http.Response, error) {
		uploads++
		if r.URL.String() != AttachmentsURL || r.Method != "POST" || r.Header.Get("Authorization") != "Bearer token" || r.Header.Get("Accept") != "application/json" {
			t.Fatalf("wrong upload request: %s %s", r.Method, r.URL)
		}
		reader, err := r.MultipartReader()
		if err != nil {
			t.Fatal(err)
		}
		part, err := reader.NextPart()
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(part)
		if err != nil || !bytes.Equal(got, image) || part.FormName() != "file" || part.Header.Get("Content-Type") != "image/png" || !strings.HasSuffix(part.FileName(), ".png") {
			t.Fatal("multipart image contract changed")
		}
		if _, err = reader.NextPart(); err != io.EOF {
			t.Fatal("unexpected multipart fields")
		}
		response := fmt.Sprintf(`{"openai_file_id":"file-%s"}`, r.Header.Get("Chatgpt-Account-Id"))
		return &http.Response{StatusCode: 200, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(response))}, nil
	})}
	for i, account := range []string{"a", "a", "b"} {
		wire, err := UploadInputImages(t.Context(), client, raw, Headers("token", account), "caller", cache)
		if err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{"input.1.content.1"} {
			part := gjson.GetBytes(wire, path)
			if part.Get("file_id").String() != "file-"+account || part.Get("image_url").Exists() {
				t.Fatalf("image not converted: %s", wire)
			}
		}
		if gjson.GetBytes(wire, "input.1.content.0.text").String() != dataURL || gjson.GetBytes(wire, "input.1.content.2.image_url").String() != "https://cdn.example/img.png" || gjson.GetBytes(wire, "input.1.content.3.file_id").String() != "existing-file" {
			t.Fatal("unrelated content changed")
		}
		if gjson.GetBytes(wire, "input.1.content.1.detail").String() != "high" || gjson.GetBytes(wire, "input.2.output.0.detail").Exists() || gjson.GetBytes(wire, "input.3.output.0.detail").String() != "original" {
			t.Fatal("image detail changed")
		}
		if gjson.GetBytes(wire, "input.2.output.0.image_url").String() != dataURL || gjson.GetBytes(wire, "input.3.output.0.image_url").String() != dataURL {
			t.Fatal("native tool-result images changed")
		}
		if gjson.GetBytes(wire, "metadata.number").Raw != "9007199254740993" || gjson.GetBytes(wire, "metadata.turn_id").String() != "same-turn" || gjson.GetBytes(wire, "input.0.reasoning.effort").String() != "max" {
			t.Fatal("request metadata or reasoning changed")
		}
		want := 1
		if i == 2 {
			want = 2
		}
		if uploads != want {
			t.Fatalf("uploads=%d want=%d", uploads, want)
		}
	}
	if gjson.GetBytes(raw, "input.1.content.1.image_url").String() != dataURL {
		t.Fatal("caller body mutated")
	}
}

func TestImagesWithoutInlineDataAreUntouched(t *testing.T) {
	raw := []byte(`{ "input":[{"role":"user","content":[{"type":"input_image","image_url":"https://cdn.example/img.png"},{"type":"input_image","file_id":"f1"},{"type":"input_text","text":"data:image/png;base64,eA=="}]}] }`)
	wire, err := UploadInputImages(t.Context(), nil, raw, nil, "", nil)
	if err != nil || !bytes.Equal(raw, wire) {
		t.Fatalf("untouched image request changed: %s %v", wire, err)
	}
}

func TestAttachmentErrorsKeepStatusAndDoNotCache(t *testing.T) {
	for _, tc := range []struct {
		status int
		body   string
		want   int
	}{
		{401, `{"error":"token secret-image"}`, 401}, {422, `{"error":"Invalid file"}`, 422}, {429, "rate limited", 429}, {503, "unavailable", 503}, {200, `{"id":"wrong-key"}`, 502}, {200, `{"openai_file_id":" "}`, 502},
	} {
		cache := &AttachmentCache{}
		calls := 0
		client := &http.Client{Transport: attachmentTransport(func(r *http.Request) (*http.Response, error) {
			calls++
			return &http.Response{StatusCode: tc.status, Body: io.NopCloser(strings.NewReader(tc.body)), Header: make(http.Header)}, nil
		})}
		raw := []byte(`{"input":[{"role":"user","content":[{"type":"input_image","image_url":"data:image/png;base64,` + base64.StdEncoding.EncodeToString([]byte("secret-image")) + `"}]}]}`)
		for i := 0; i < 2; i++ {
			_, err := UploadInputImages(t.Context(), client, raw, Headers("token", "account"), "caller", cache)
			var status interface{ StatusCode() int }
			if !errors.As(err, &status) || status.StatusCode() != tc.want || strings.Contains(err.Error(), "secret-image") || strings.Contains(err.Error(), "token") {
				t.Fatalf("status=%d error=%v", tc.status, err)
			}
		}
		if calls != 2 {
			t.Fatal("failed upload was cached")
		}
	}
}

func TestAttachmentCacheConcurrentUploadAndCancellation(t *testing.T) {
	cache := &AttachmentCache{}
	entered, release := make(chan struct{}), make(chan struct{})
	var uploads atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		fileID, err := cache.getOrUpload(t.Context(), "image", func() (string, error) { uploads.Add(1); close(entered); <-release; return "file", nil })
		if err != nil || fileID != "file" {
			t.Errorf("upload failed: %s %v", fileID, err)
		}
	}()
	<-entered
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := cache.getOrUpload(ctx, "image", func() (string, error) { t.Error("duplicate upload"); return "", nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel error=%v", err)
	}
	close(release)
	wg.Wait()
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := cache.getOrUpload(t.Context(), "image", func() (string, error) { uploads.Add(1); return "duplicate", nil })
			if err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if uploads.Load() != 1 {
		t.Fatal("upload not deduplicated")
	}
}

func TestDecodeInlineImageFormats(t *testing.T) {
	for _, dataURL := range []string{"data:image/png;base64,eA==", "data:image/png;base64,eA%3D%3D", "data:image/png,x", "DATA:image/png;BASE64,eA=="} {
		kind, data, err := decodeInlineImage(dataURL)
		if err != nil || kind != "image/png" || string(data) != "x" {
			t.Fatalf("decode %q: %s %q %v", dataURL, kind, data, err)
		}
	}
	for _, dataURL := range []string{"data:image/png;base64,", "data:image/png;base64,???", "data:image/png;base64", "data:text/plain,x", "data:image/png,%xy"} {
		if _, _, err := decodeInlineImage(dataURL); err == nil {
			t.Fatalf("invalid data URL accepted: %s", dataURL)
		}
	}
}
