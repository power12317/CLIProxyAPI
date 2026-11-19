package executor

import (
	"context"

	"github.com/MoeForge/moego/ojson"
)

// SessionInstructionMarker looks at the original JSON members so that duplicate
// fields cannot disappear when a WebSocket frame is normalized for execution.
func SessionInstructionMarker(root ojson.Result) (marker ojson.Result, ambiguous bool) {
	metadataCount, markerCount := 0, 0
	root.ForEach(func(key, value ojson.Result) bool {
		if key.String() == "client_metadata" {
			metadataCount++
			if value.IsObject() {
				value.ForEach(func(k, v ojson.Result) bool {
					if k.String() == "cpa_session_instructions" {
						marker, markerCount = v, markerCount+1
					}
					return true
				})
			}
		}
		return true
	})
	return marker, markerCount > 1 || (markerCount > 0 && metadataCount > 1)
}

// UniqueSessionInstructionFields checks only the fields used as continuation evidence.
func UniqueSessionInstructionFields(root ojson.Result, fields ...string) bool {
	var seen uint32
	unique := true
	root.ForEach(func(key, _ ojson.Result) bool {
		for i, field := range fields {
			if key.String() == field {
				bit := uint32(1) << i
				if seen&bit != 0 {
					unique = false
					return false
				}
				seen |= bit
				break
			}
		}
		return true
	})
	return unique
}

type implicitSessionInstructionReuseKey struct{}

func WithImplicitSessionInstructionReuse(ctx context.Context) context.Context {
	return context.WithValue(ctx, implicitSessionInstructionReuseKey{}, true)
}

func ImplicitSessionInstructionReuse(ctx context.Context) bool {
	v, _ := ctx.Value(implicitSessionInstructionReuseKey{}).(bool)
	return v
}
