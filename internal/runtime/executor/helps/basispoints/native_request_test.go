package basispoints

import (
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestNativeAccountHeaders(t *testing.T) {
	for _, tc := range []struct{ name, claims, user string }{
		{"both", `{"chatgpt_account_id":"account","chatgpt_account_user_id":"account-user","chatgpt_plan_type":"pro","organizations":[{"id":"org"}],"is_enterprise":false}`, "account-user"},
		{"missing-user", `{"chatgpt_account_id":"account"}`, ""},
		{"invalid-user", `{"chatgpt_account_id":"account","chatgpt_account_user_id":42}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token := "header." + base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":`+tc.claims+`}`)) + ".signature"
			headers := Headers(token, AccountID(token))
			if headers.Get("ChatGPT-Account-ID") != "account" || headers.Get("X-OpenAI-Account-User-Id") != tc.user {
				t.Fatalf("wrong account claims: account=%s user=%s", headers.Get("ChatGPT-Account-ID"), headers.Get("X-OpenAI-Account-User-Id"))
			}
			if tc.user == "" && len(headers.Values("X-OpenAI-Account-User-Id")) != 0 {
				t.Fatal("empty account user header must be omitted")
			}
		})
	}
	if AccountID("opaque-token") != "" || Headers("opaque-token", "account").Get("X-OpenAI-Account-User-Id") != "" {
		t.Fatal("opaque token produced a fabricated account claim")
	}
}

func TestNativeToolsVersionDefaultAndOverride(t *testing.T) {
	for _, version := range []string{"", "tools-explicit-client-version"} {
		raw, _ := json.Marshal(object{"model": "original", "input": "hello", "metadata": object{"bps_tools_version_id": version}})
		body, _, err := Prepare(raw, "scope", "session", &Cache{})
		if err != nil {
			t.Fatal(err)
		}
		want := version
		if want == "" {
			want = ToolsVersionID
		}
		if gjson.GetBytes(body, "metadata.bps_tools_version_id").String() != want {
			t.Fatalf("tools version not selected: %s", body)
		}
		for _, key := range []string{"context_management", "prompt_cache_key", "service_tier"} {
			if gjson.GetBytes(body, key).Exists() {
				t.Fatalf("unrequested field added: %s", key)
			}
		}
	}
}

func TestAgentIterationCountsParallelToolRounds(t *testing.T) {
	call := func(id string) object {
		return object{"type": "function_call", "name": "weather", "call_id": id, "arguments": "{}"}
	}
	output := func(id string) object { return object{"type": "function_call_output", "call_id": id, "output": "ok"} }
	input := []any{message("user", "hello")}
	var turnID string
	for round := 1; round <= 3; round++ {
		raw, _ := json.Marshal(object{"model": "original", "input": input, "tools": []any{object{"type": "function", "name": "weather"}}})
		for retry := 0; retry < 2; retry++ {
			body, _, err := Prepare(raw, "scope", "session", &Cache{})
			if err != nil {
				t.Fatal(err)
			}
			if got := gjson.GetBytes(body, "metadata.agent_iteration").Int(); got != int64(round) {
				t.Fatalf("round=%d retry=%d iteration=%d", round, retry, got)
			}
			id := gjson.GetBytes(body, "metadata.turn_id").String()
			if turnID == "" {
				turnID = id
			}
			if id != turnID {
				t.Fatal("turn_id changed within a user turn")
			}
		}
		if round == 1 {
			input = append(input, call("a"), call("b"), output("a"), output("b"))
		} else {
			input = append(input, call("c"), output("c"))
		}
	}
	input = append(input, message("user", "next turn"))
	raw, _ := json.Marshal(object{"model": "original", "input": input})
	body, _, err := Prepare(raw, "scope", "session", &Cache{})
	if err != nil || gjson.GetBytes(body, "metadata.agent_iteration").String() != "1" || gjson.GetBytes(body, "metadata.turn_id").String() == turnID {
		t.Fatalf("new turn not reset: %s %v", body, err)
	}
}
