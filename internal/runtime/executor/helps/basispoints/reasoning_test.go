package basispoints

import (
	"encoding/json"
	"testing"

	"github.com/MoeForge/moego/ojson"
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
			if ojson.GetBytes(wire, "reasoning_effort").String() != want || ojson.GetBytes(wire, "model").String() != model {
				t.Fatalf("model=%s effort=%s: %s", model, effort, wire)
			}
			if effort == "" && ojson.GetBytes(wire, "reasoning_effort").Exists() {
				t.Fatal("default effort inserted")
			}
			updates := 0
			for _, item := range ojson.GetBytes(wire, "input").Array() {
				if item.Get("type").String() == "configuration_update" && item.Get("reasoning.effort").String() == "max" {
					updates++
				}
			}
			if (special && updates != 1) || (!special && updates != 0) {
				t.Fatalf("model=%s effort=%s updates=%d", model, effort, updates)
			}
			instructionPath := "input.0.content.0.text"
			if special {
				if ojson.GetBytes(wire, "input.0.type").String() != "configuration_update" || ojson.GetBytes(wire, "input.0.reasoning.effort").String() != "max" {
					t.Fatal("max configuration_update must lead input")
				}
				instructionPath = "input.1.content.0.text"
			}
			if ojson.GetBytes(wire, instructionPath).String() != "Keep this instruction." {
				t.Fatal("caller instructions changed")
			}
			if ojson.GetBytes(raw, "reasoning_effort").String() != effort {
				t.Fatal("caller body changed")
			}
		}
	}
}
