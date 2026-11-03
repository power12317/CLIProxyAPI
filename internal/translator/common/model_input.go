package common

import (
	"context"
	"strings"

	"github.com/MoeForge/moego/ojson"
)

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

// PreserveModelInputFields carries fields not consumed by a source-protocol
// conversion into native model input. The native parser owns their types and
// capabilities; a second list of supported native fields would silently lag it.
// Explicit native values take precedence over translated aliases and defaults.
func PreserveModelInputFields(converted []byte, source ojson.Result, consumed ...string) []byte {
	doc := ojson.NewDoc(converted)
	PreserveModelInputFieldsDoc(doc, source, consumed...)
	return doc.Bytes()
}

// PreserveModelInputFieldsDoc carries unconsumed native fields into one document.
func PreserveModelInputFieldsDoc(doc *ojson.Doc, source ojson.Result, consumed ...string) {
	if doc == nil {
		return
	}
	source.ForEach(func(key, value ojson.Result) bool {
		for _, name := range consumed {
			if key.String() == name {
				return true
			}
		}
		path := strings.NewReplacer("\\", "\\\\", ".", "\\.", ":", "\\:").Replace(key.String())
		current := doc.Get(path)
		raw := []byte(value.Raw)
		if current.IsObject() && value.IsObject() {
			raw = PreserveModelInputFields([]byte(current.Raw), value)
		}
		_ = doc.SetRawBytes(path, raw)
		return true
	})
}
