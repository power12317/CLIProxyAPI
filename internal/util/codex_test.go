package util

import "testing"

func TestClassifyCodexResponsesLiteTools(t *testing.T) {
	tests := []struct {
		name string
		body string
		want CodexResponsesLiteToolMode
	}{
		{name: "unknown without tools", body: `{"input":[]}`, want: CodexResponsesLiteToolsUnknown},
		{name: "empty tools", body: `{"tools":[]}`, want: CodexResponsesLiteToolsCompatible},
		{name: "function", body: `{"tools":[{"type":"function","name":"exec"}]}`, want: CodexResponsesLiteToolsCompatible},
		{name: "custom", body: `{"tools":[{"type":"custom","name":"exec"}]}`, want: CodexResponsesLiteToolsCompatible},
		{name: "client tool search", body: `{"tools":[{"type":"tool_search","execution":"client"}]}`, want: CodexResponsesLiteToolsCompatible},
		{name: "namespace", body: `{"input":[{"type":"additional_tools","tools":[{"type":"namespace","name":"functions","tools":[{"type":"function","name":"exec"}]}]}]}`, want: CodexResponsesLiteToolsCompatible},
		{name: "hosted image", body: `{"tools":[{"type":"image_generation"}]}`, want: CodexResponsesLiteToolsIncompatible},
		{name: "mixed tools", body: `{"tools":[{"type":"function","name":"exec"},{"type":"web_search"}]}`, want: CodexResponsesLiteToolsIncompatible},
		{name: "server tool search", body: `{"tools":[{"type":"tool_search","execution":"server"}]}`, want: CodexResponsesLiteToolsIncompatible},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ClassifyCodexResponsesLiteTools([]byte(test.body)); got != test.want {
				t.Fatalf("classification = %v, want %v", got, test.want)
			}
		})
	}
}
