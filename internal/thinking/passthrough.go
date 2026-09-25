package thinking

import "fmt"

// ApplyThinkingPassthrough retains the caller's effort without model-capability
// validation. Extraction and suffix precedence still use the canonical pipeline;
// the destination applier alone controls the output wire format.
func ApplyThinkingPassthrough(body, sourceBody []byte, model, fromFormat, toFormat string) ([]byte, error) {
	applier := GetProviderApplier(toFormat)
	if applier == nil {
		return body, fmt.Errorf("thinking: no applier for %q", toFormat)
	}
	var config ThinkingConfig
	suffix := ParseSuffix(model)
	if suffix.HasSuffix {
		config = parseSuffixToConfig(suffix.RawSuffix, toFormat, model)
	} else {
		config = extractSourceThinkingConfig(sourceBody, fromFormat)
		if !hasThinkingConfig(config) && (fromFormat == "codex" || fromFormat == "openai-response") {
			config = extractOpenAIConfig(sourceBody)
		}
	}
	return applier.Apply(body, config, nil)
}
