// Package basispoints translates canonical thinking into the Basispoints wire format.
package basispoints

import (
	"github.com/router-for-me/CLIProxyAPI/v7/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v7/internal/thinking"
	"github.com/tidwall/sjson"
)

type Applier struct{}

func init() { thinking.RegisterProvider("basispoints", Applier{}) }

// Apply preserves level values and leaves upstream validation to Basispoints.
func (Applier) Apply(body []byte, config thinking.ThinkingConfig, _ *registry.ModelInfo) ([]byte, error) {
	var err error
	body, err = sjson.DeleteBytes(body, "reasoning.effort")
	if err != nil {
		return body, err
	}
	body, err = sjson.DeleteBytes(body, "reasoning_effort")
	if err != nil {
		return body, err
	}
	var value any
	switch config.Mode {
	case thinking.ModeLevel:
		if config.Level != "" {
			value = string(config.Level)
		}
	case thinking.ModeNone:
		value = "none"
	case thinking.ModeAuto:
		value = "auto"
	case thinking.ModeBudget:
		if config.Budget != 0 {
			value = config.Budget
		}
	}
	if value == nil {
		return body, nil
	}
	return sjson.SetBytes(body, "reasoning_effort", value)
}
