package basispoints

import (
	"encoding/json"
	"testing"

	"github.com/tidwall/gjson"
)

func TestAstraMaxReasoningAdaptation(t *testing.T) {
	for _, model := range []string{"gpt-6-astra", "gpt-6-luna", "future-model"} {
		for _, effort := range []string{"", "low", "xhigh", "max", "ultra"} {
			body := object{"model": model, "input": "hello", "instructions": "Keep this instruction."}
			if effort != "" {
				body["reasoning_effort"] = effort
			}
			raw, _ := json.Marshal(body)
			wire, _, err := Prepare(raw, "scope", "session", &Cache{})
			if err != nil {
				t.Fatal(err)
			}
			want := effort
			special := model == "gpt-6-astra" && effort == "max"
			if special {
				want = "xhigh"
			}
			if gjson.GetBytes(wire, "reasoning_effort").String() != want || gjson.GetBytes(wire, "model").String() != model {
				t.Fatalf("model=%s effort=%s: %s", model, effort, wire)
			}
			if effort == "" && gjson.GetBytes(wire, "reasoning_effort").Exists() {
				t.Fatal("default effort inserted")
			}
			updates := 0
			for _, item := range gjson.GetBytes(wire, "input").Array() {
				if item.Get("type").String() == "configuration_update" && item.Get("reasoning.effort").String() == "max" {
					updates++
				}
			}
			if (special && updates != 1) || (!special && updates != 0) {
				t.Fatalf("model=%s effort=%s updates=%d", model, effort, updates)
			}
			instructionPath := "input.0.content.0.text"
			if special {
				if gjson.GetBytes(wire, "input.0.type").String() != "configuration_update" || gjson.GetBytes(wire, "input.0.reasoning.effort").String() != "max" {
					t.Fatal("max configuration_update must lead input")
				}
				instructionPath = "input.1.content.0.text"
			}
			if gjson.GetBytes(wire, instructionPath).String() != "Keep this instruction." {
				t.Fatal("caller instructions changed")
			}
			if gjson.GetBytes(raw, "reasoning_effort").String() != effort {
				t.Fatal("caller body changed")
			}
		}
	}
}
