package basispoints

// adaptReasoning implements the explicit Astra-only Basispoints adaptation.
// The wire effort stays xhigh while an input configuration_update requests max.
// Native Codex requests and the canonical suffix parser do not use this adapter.
func adaptReasoning(body object) (any, bool) {
	effort := body["reasoning_effort"]
	if body["model"] == "gpt-6-astra" && effort == "max" {
		return "xhigh", true
	}
	return effort, false
}
