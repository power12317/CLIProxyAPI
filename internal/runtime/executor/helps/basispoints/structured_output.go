package basispoints

import (
	"encoding/json"
	"fmt"
	"io"
	"regexp"
	"strings"

	"github.com/MoeForge/moego/ojson"
	"github.com/santhosh-tekuri/jsonschema/v6"
)

const localSchemaURL = "https://basispoints.invalid/output-schema.json"

var outputFormatName = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// Structured output is prompted and verified locally; BPS does not expose
// constrained decoding. Message events must wait for the terminal response.
type structuredOutput struct {
	format object
	schema *jsonschema.Schema
}

type closedSchemaLoader struct{}

func (closedSchemaLoader) Load(string) (any, error) {
	return nil, fmt.Errorf("external schema resources are not supported")
}

// Untrusted schemas may resolve local references only, never network or files.
func compileLocalSchema(schema object) (*jsonschema.Schema, error) {
	raw, err := json.Marshal(schema)
	if err != nil || len(raw) > 1<<20 {
		return nil, fmt.Errorf("schema exceeds the 1 MiB limit")
	}
	compiler := jsonschema.NewCompiler()
	compiler.DefaultDraft(jsonschema.Draft2020)
	compiler.AssertFormat()
	compiler.UseLoader(closedSchemaLoader{})
	if err = compiler.AddResource(localSchemaURL, schema); err != nil {
		return nil, fmt.Errorf("invalid JSON schema")
	}
	compiled, err := compiler.Compile(localSchemaURL)
	if err != nil {
		return nil, fmt.Errorf("invalid JSON schema or external schema reference")
	}
	return compiled, nil
}

func prepareStructuredOutput(value any) (*structuredOutput, error) {
	if value == nil {
		return nil, nil
	}
	config, ok := value.(object)
	if !ok {
		return nil, failure(400, "invalid_text_format", "text must be an object")
	}
	raw, present := config["format"]
	if !present {
		return nil, nil
	}
	format, ok := raw.(object)
	if !ok {
		return nil, failure(400, "invalid_text_format", "text.format must be an object")
	}
	kind := stringValue(format["type"])
	if kind != "text" && kind != "json_object" && kind != "json_schema" {
		return nil, failure(400, "invalid_text_format", "text.format requires text, json_object or json_schema")
	}
	for key := range format {
		if key == "type" || kind == "json_schema" && (key == "name" || key == "schema" || key == "strict" || key == "description") {
			continue
		}
		return nil, failure(400, "invalid_text_format", "text.format contains an unsupported field")
	}
	if kind == "text" {
		return nil, nil
	}
	result := &structuredOutput{format: clone(format)}
	if kind == "json_object" {
		return result, nil
	}
	if !outputFormatName.MatchString(stringValue(format["name"])) {
		return nil, failure(400, "invalid_text_format", "json_schema requires a valid format name")
	}
	if strict, present := format["strict"]; present {
		if _, ok := strict.(bool); !ok {
			return nil, failure(400, "invalid_text_format", "json_schema strict must be a boolean")
		}
	}
	if description, present := format["description"]; present {
		if _, ok := description.(string); !ok {
			return nil, failure(400, "invalid_text_format", "json_schema description must be a string")
		}
	}
	schema, ok := format["schema"].(object)
	if !ok {
		return nil, failure(400, "invalid_text_format", "json_schema requires a schema object")
	}
	var err error
	result.schema, err = compileLocalSchema(schema)
	if err != nil {
		return nil, failure(400, "invalid_text_format", err.Error())
	}
	return result, nil
}

func (s *structuredOutput) instructions() string {
	text := "The final assistant answer must be exactly one JSON value without Markdown fences or surrounding prose. The gateway validates the final answer before returning message text. Tool calls and refusals are separate protocol items; use declared tools when needed."
	if s.schema == nil {
		return text + " The JSON value must be an object."
	}
	format, _ := json.Marshal(s.format)
	return text + " Satisfy this client output format: " + string(format)
}

func (s *structuredOutput) validate(response object) error {
	invalid := func() error {
		return failure(502, "invalid_structured_output", "Basispoints final answer does not satisfy the requested JSON format")
	}
	items, ok := response["output"].([]any)
	if !ok {
		return invalid()
	}
	var answer strings.Builder
	toolTurn, refusal := false, false
	for _, value := range items {
		item, ok := value.(object)
		if !ok {
			return invalid()
		}
		toolTurn = toolTurn || isTool(item)
		if item["type"] != "message" {
			continue
		}
		parts, ok := item["content"].([]any)
		if !ok {
			return invalid()
		}
		for _, value := range parts {
			part, ok := value.(object)
			if !ok {
				return invalid()
			}
			switch part["type"] {
			case "output_text":
				text, ok := part["text"].(string)
				if !ok || answer.Len()+len(text) > maxItemBytes {
					return invalid()
				}
				answer.WriteString(text)
			case "refusal":
				if _, ok := part["refusal"].(string); !ok {
					return invalid()
				}
				refusal = true
			default:
				return invalid()
			}
		}
	}
	if toolTurn || refusal && answer.Len() == 0 {
		return nil
	}
	// An incomplete response has no final answer to validate, but its partial
	// message must not escape as though it had satisfied the output contract.
	if status := stringValue(response["status"]); status != "" && status != "completed" {
		return invalid()
	}
	decoder := json.NewDecoder(strings.NewReader(answer.String()))
	decoder.UseNumber()
	var value, extra any
	if decoder.Decode(&value) != nil || decoder.Decode(&extra) != io.EOF {
		return invalid()
	}
	if s.schema == nil {
		if _, ok := value.(map[string]any); !ok {
			return invalid()
		}
	} else if s.schema.Validate(value) != nil {
		return invalid()
	}
	return nil
}

func (s *structuredOutput) decorate(response object) {
	config, _ := response["text"].(object)
	if config == nil {
		config = object{}
		response["text"] = config
	}
	config["format"] = clone(s.format)
}

func structuredMessageEvent(kind string, item object) bool {
	return strings.HasPrefix(kind, "response.output_text.") || strings.HasPrefix(kind, "response.refusal.") ||
		strings.HasPrefix(kind, "response.content_part.") ||
		strings.HasPrefix(kind, "response.output_item.") && item["type"] == "message"
}

// Only authoritative, validated terminal message items are reconstructed.
func emitStructuredMessage(item object, index int, send func(object) error) error {
	added := clone(item)
	added["content"], added["status"] = []any{}, "in_progress"
	if err := send(object{"type": "response.output_item.added", "output_index": index, "item": added}); err != nil {
		return err
	}
	parts, _ := item["content"].([]any)
	for i, value := range parts {
		part := value.(object)
		field, prefix := "text", "response.output_text"
		if part["type"] == "refusal" {
			field, prefix = "refusal", "response.refusal"
		}
		empty := clone(part)
		empty[field] = ""
		for _, event := range []object{
			{"type": "response.content_part.added", "part": empty},
			{"type": prefix + ".delta", "delta": part[field]},
			{"type": prefix + ".done", field: part[field]},
			{"type": "response.content_part.done", "part": part},
		} {
			event["output_index"], event["item_id"], event["content_index"] = index, item["id"], i
			if err := send(event); err != nil {
				return err
			}
		}
	}
	return send(object{"type": "response.output_item.done", "output_index": index, "item": item})
}

// PreserveChatOutputFormat retains the client's explicit format before the BPS
// envelope is built. The generic Codex translator can omit json_object mode.
func PreserveChatOutputFormat(doc *ojson.Doc, source []byte) error {
	body, err := decode(source)
	if err != nil {
		return err
	}
	value, present := body["response_format"]
	if !present {
		return nil
	}
	format, ok := value.(object)
	if !ok {
		return failure(400, "invalid_text_format", "response_format must be an object")
	}
	if format["type"] == "json_schema" {
		for key := range format {
			if key != "type" && key != "json_schema" {
				return failure(400, "invalid_text_format", "response_format contains an unsupported field")
			}
		}
		schema, ok := format["json_schema"].(object)
		if !ok {
			return failure(400, "invalid_text_format", "response_format.json_schema must be an object")
		}
		format = clone(schema)
		format["type"] = "json_schema"
	}
	return doc.Set("text.format", format)
}
