package basispoints

import (
	"fmt"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/gjson"
)

func TestPassthroughKeepsEffortAndSuffixPrecedence(t *testing.T) {
	for _, effort := range []string{"low", "medium", "high", "xhigh", "max", "ultra", "future-effort"} {
		t.Run(effort, func(t *testing.T) {
			source := []byte(fmt.Sprintf(`{"reasoning":{"effort":%q}}`, effort))
			body, err := thinking.ApplyThinkingPassthrough([]byte(`{"reasoning":{"effort":"medium"},"reasoning_effort":"medium"}`), source, "gpt-6-astra", "openai-response", "basispoints")
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(body, "reasoning_effort").String(); got != effort {
				t.Fatalf("effort = %q, want %q", got, effort)
			}
			if gjson.GetBytes(body, "reasoning.effort").Exists() {
				t.Fatal("nested effort remains")
			}
		})
	}
	body, err := thinking.ApplyThinkingPassthrough([]byte(`{}`), []byte(`{"reasoning":{"effort":"low"}}`), "gpt-6-astra(high)", "openai-response", "basispoints")
	if err != nil {
		t.Fatal(err)
	}
	if got := gjson.GetBytes(body, "reasoning_effort").String(); got != "high" {
		t.Fatalf("suffix effort = %q", got)
	}
	if _, supported := thinking.ParseLevelSuffix("ultra"); supported {
		t.Fatal("Basispoints must not extend the existing suffix parser")
	}
	body, err = thinking.ApplyThinkingPassthrough([]byte(`{"reasoning":{"effort":"medium"}}`), []byte(`{}`), "gpt-6-astra", "openai-response", "basispoints")
	if err != nil {
		t.Fatal(err)
	}
	if gjson.GetBytes(body, "reasoning_effort").Exists() || gjson.GetBytes(body, "reasoning.effort").Exists() {
		t.Fatalf("injected default remains: %s", body)
	}
}
