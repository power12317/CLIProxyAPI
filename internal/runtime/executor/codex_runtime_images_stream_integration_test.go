package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	bridge "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps/codexruntime"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type nativeRuntimeImageProbe struct {
	release chan struct{}
	stop    chan struct{}
	once    sync.Once
}

func newNativeRuntimeImageProbe() *nativeRuntimeImageProbe {
	return &nativeRuntimeImageProbe{release: make(chan struct{}, 2), stop: make(chan struct{})}
}

func (p *nativeRuntimeImageProbe) close() { p.once.Do(func() { close(p.stop) }) }

func (p *nativeRuntimeImageProbe) serve(ctx context.Context, w http.ResponseWriter, path string, gated bool) {
	prefix := "image_generation"
	if path == codexImagesEditsPath {
		prefix = "image_edit"
	}
	w.Header().Set("Content-Type", "text/event-stream")
	for index := range 2 {
		_, _ = fmt.Fprintf(w, "event: %s.partial_image\ndata: {\"type\":\"%s.partial_image\",\"partial_image_index\":%d,\"b64_json\":\"native-image-partial-%d\"}\n\n", prefix, prefix, index, index+1)
		w.(http.Flusher).Flush()
		if gated {
			select {
			case <-p.release:
			case <-ctx.Done():
				return
			case <-p.stop:
				return
			}
		}
	}
	_, _ = fmt.Fprintf(w, "event: %s.completed\ndata: {\"type\":\"%s.completed\",\"b64_json\":\"real-worker-image\",\"created_at\":1,\"output_format\":\"jpeg\"}\n\n", prefix, prefix)
	w.(http.Flusher).Flush()
}

func testNativeRuntimeLiveImage(t *testing.T, router http.Handler, probe *nativeRuntimeImageProbe, path string) {
	t.Helper()
	server := httptest.NewServer(router)
	defer server.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 15*time.Second)
	defer cancel()
	payload, _ := nativeRuntimeImagePayload(t, path, true, false)
	payload, _ = sjson.SetBytes(payload, "prompt", "live-image-stream")
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+path, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	transport := &http.Transport{}
	defer transport.CloseIdleConnections()
	started := time.Now()
	response, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = response.Body.Close() }()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("image HTTP status=%d", response.StatusCode)
	}
	reader := bridge.NewSSEReader(response.Body)
	for index := range 2 {
		frame, errNext := reader.Next()
		if errNext != nil {
			t.Fatal(errNext)
		}
		if !strings.HasSuffix(gjson.GetBytes(frame.Data, "type").String(), ".partial_image") || gjson.GetBytes(frame.Data, "b64_json").String() != fmt.Sprintf("native-image-partial-%d", index+1) {
			t.Fatalf("image increment was not delivered before its next phase: %s", frame.Data)
		}
		t.Logf("client image event %d arrived at %s while upstream completion was blocked", index+1, time.Since(started))
		probe.release <- struct{}{}
	}
	frame, err := reader.Next()
	if err != nil || !strings.HasSuffix(gjson.GetBytes(frame.Data, "type").String(), ".completed") {
		t.Fatalf("image completion=%s error=%v", frame.Data, err)
	}
}
