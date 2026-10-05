package executor

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	core "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/tidwall/gjson"
)

// This fixture runs the actual adapter HTTP/parser/tool-state boundary. Only the
// upstream browser is replaced; the ordinary suite does not require Python.
func TestPrismActualAdapterToolRoundTrip(t *testing.T) {
	python := os.Getenv("PRISM_TEST_PYTHON")
	if python == "" {
		t.Skip("set PRISM_TEST_PYTHON to the adapter virtualenv interpreter")
	}
	root, err := filepath.Abs("../../../prism-adapter")
	if err != nil {
		t.Fatal(err)
	}
	fixture := `
import sys, tempfile, re, json, threading
sys.path.insert(0, sys.argv[1])
import server
from tool_state import ToolState
class Browser:
    count=0
    def run(self, account, token, prompt, session=None, model=None, effort='medium', reuse_project=True):
        self.count+=1
        assert len(account)==64
        marker=re.search(r'PRISM_CLIENT_TOOLS_V1:[0-9a-f]+',prompt)
        if marker is None: return str(self.count),'text result'
        assert not reuse_project
        results=prompt.count('CLIENT_TOOL_RESULT')
        value=({'kind':'calls','calls':[{'name':'lookup','arguments':{'key':'first'}}]} if results==0 else
               {'kind':'calls','calls':[{'name':'echo','input':'line 1\nline 2'}]} if results==1 else
               {'kind':'final','text':'complete'})
        return str(self.count),marker.group(0)+'\n'+json.dumps(value)
state=tempfile.TemporaryDirectory()
server.Handler.api_key='bridge-key'
server.Handler.tool_state=ToolState(state.name,server.AdapterError)
server.Handler.browser_turn=Browser()
httpd=server.ThreadingHTTPServer(('127.0.0.1',0),server.Handler)
print(httpd.server_address[1],flush=True)
httpd.serve_forever()
`
	cmd := exec.CommandContext(t.Context(), python, "-B", "-u", "-c", fixture, root)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	cmd.Stderr = os.Stderr
	if err = cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	scanner := bufio.NewScanner(stdout)
	if !scanner.Scan() {
		t.Fatal("adapter did not start")
	}
	endpoint := "http://127.0.0.1:" + strings.TrimSpace(scanner.Text())
	t.Setenv("PRISM_ADAPTER_API_KEY", "bridge-key")
	executor := NewPrismExecutor(prismConfig(t, endpoint))
	opts := prismOptions()
	req := core.Request{Model: "gpt-6.1-sol", Payload: []byte(`{"model":"gpt-6.1-sol","input":"hello"}`)}
	if _, err = executor.Execute(t.Context(), prismTestAuth(), req, opts); err != nil {
		t.Fatal(err)
	}
	tools := []any{
		map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object", "properties": map[string]any{"key": map[string]any{"type": "string"}}, "required": []string{"key"}, "additionalProperties": false}},
		map[string]any{"type": "custom", "name": "echo", "format": map[string]any{"type": "text"}},
	}
	history := []any{map[string]any{"role": "user", "content": "Use client tools then answer."}}
	for turn := 0; turn < 3; turn++ {
		payload, _ := json.Marshal(map[string]any{"model": "gpt-6.1-sol", "input": history, "tools": tools, "reasoning": map[string]any{"effort": "high", "summary": "auto"}, "include": []string{"reasoning.encrypted_content"}, "store": false})
		req.Payload = payload
		result, err := executor.ExecuteStream(t.Context(), prismTestAuth(), req, opts)
		if err != nil {
			t.Fatalf("turn %d: %v", turn, err)
		}
		var terminal gjson.Result
		for chunk := range result.Chunks {
			for _, line := range strings.Split(string(chunk.Payload), "\n") {
				if strings.HasPrefix(line, "data:") {
					event := gjson.Parse(strings.TrimSpace(strings.TrimPrefix(line, "data:")))
					if event.Get("type").String() == "response.completed" {
						terminal = event.Get("response")
					}
				}
			}
		}
		item := terminal.Get("output.0")
		if turn == 2 {
			if item.Get("content.0.text").String() != "complete" {
				t.Fatalf("not complete: %s", terminal.Raw)
			}
			break
		}
		kind := "function_call"
		if turn == 1 {
			kind = "custom_tool_call"
			if item.Get("input").String() != "line 1\nline 2" {
				t.Fatal("custom input changed")
			}
		}
		if item.Get("type").String() != kind {
			t.Fatalf("turn %d: %s", turn, item.Raw)
		}
		history = append(history, item.Value(), map[string]any{"type": kind + "_output", "call_id": item.Get("call_id").String(), "output": fmt.Sprintf("result %d", turn)})
	}
}
