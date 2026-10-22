package common

import "context"

type modelInputKey struct{}

// WithModelInput identifies a business-input destination whose native executor
// owns wire defaults and capabilities. It is request-local, not a user setting.
func WithModelInput(ctx context.Context) context.Context {
	return context.WithValue(ctx, modelInputKey{}, true)
}

func IsModelInput(ctx context.Context) bool {
	value, _ := ctx.Value(modelInputKey{}).(bool)
	return value
}
