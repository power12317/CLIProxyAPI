package helps

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strconv"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/util"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const codexLiteImageNamespace = `{"type":"namespace","name":"image_gen","description":"Tools in the image_gen namespace.","tools":[{"type":"function","name":"imagegen","description":"Generate an image from a prompt. Image editing requires an executor that can resolve the supplied local references. Do not combine the two reference-selection options.","strict":false,"parameters":{"type":"object","properties":{"prompt":{"type":"string"},"referenced_image_paths":{"type":["array","null"],"items":{"type":"string"},"maxItems":5},"num_last_images_to_include":{"type":["integer","null"],"minimum":1,"maximum":5}},"required":["prompt"],"additionalProperties":false}}]}`
const codexLiteWebNamespace = `{"type":"namespace","name":"web","description":"Tools in the web namespace.","tools":[{"type":"function","name":"run","description":"Search the web for the supplied queries and return source information through the connected search executor.","strict":false,"parameters":{"type":"object","properties":{"search_query":{"type":"array","minItems":1,"items":{"type":"object","properties":{"q":{"type":"string"},"recency":{"type":"integer","minimum":0},"domains":{"type":"array","items":{"type":"string"}}},"required":["q"],"additionalProperties":false}},"response_length":{"type":"string","enum":["short","medium","long"]}},"required":["search_query"],"additionalProperties":false}}]}`

// CodexImageTool recognizes hosted, flattened and namespaced image declarations.
func CodexImageTool(tool gjson.Result, namespace string) bool {
	name := tool.Get("name").String()
	if name == "" {
		name = tool.Get("function.name").String()
	}
	if namespace == "" {
		namespace = tool.Get("namespace").String()
	}
	return tool.Get("type").String() == "image_generation" || name == "image_gen.imagegen" || name == "image_gen__imagegen" || namespace == "image_gen" && name == "imagegen"
}

func codexToolDeclarationLists(body []byte) []gjson.Result {
	lists := []gjson.Result{gjson.GetBytes(body, "tools")}
	for _, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() == "additional_tools" {
			lists = append(lists, item.Get("tools"))
		}
	}
	return lists
}

// HasCodexImageTool checks declarations in both top-level and additional_tools lists.
func HasCodexImageTool(body []byte) bool {
	for _, list := range codexToolDeclarationLists(body) {
		for _, tool := range list.Array() {
			if CodexImageTool(tool, "") {
				return true
			}
			if tool.Get("type").String() == "namespace" {
				for _, child := range tool.Get("tools").Array() {
					if CodexImageTool(child, tool.Get("name").String()) {
						return true
					}
				}
			}
		}
	}
	return false
}

func codexHasHostedTool(body []byte, types ...string) bool {
	for _, list := range codexToolDeclarationLists(body) {
		for _, tool := range list.Array() {
			for _, typ := range types {
				if tool.Get("type").String() == typ {
					return true
				}
			}
		}
	}
	return false
}

func codexLiteItemID(body []byte, prefix string, content []byte) string {
	thread := gjson.GetBytes(body, "client_metadata.thread_id").String()
	if thread == "" {
		thread = gjson.Parse(gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").String()).Get("thread_id").String()
	}
	if thread == "" {
		thread = gjson.GetBytes(body, "prompt_cache_key").String()
	}
	sum := sha256.Sum256(append([]byte(thread+"\x00"), content...))
	return prefix + "_" + hex.EncodeToString(sum[:16])
}

func codexDeclaredFunction(body []byte, namespace, name string) bool {
	_, declaredName := codexLiteFunctionIdentity(body, namespace, name)
	return declaredName != ""
}

func codexLiteFunctionIdentity(body []byte, namespace, name string) (string, string) {
	for _, tool := range util.CollectResponsesToolDescriptors(gjson.ParseBytes(body)) {
		if tool.ToolType == "function" && (tool.Name == namespace+"__"+name || tool.Name == namespace+"."+name || tool.Namespace == namespace && tool.LocalName == name) {
			return tool.Namespace, tool.LocalName
		}
	}
	return "", ""
}

func codexLiteFunctionReference(body []byte, namespace, name string) []byte {
	declaredNamespace, declaredName := codexLiteFunctionIdentity(body, namespace, name)
	if declaredName == "" {
		return nil
	}
	choice := []byte(`{"type":"function"}`)
	choice, _ = sjson.SetBytes(choice, "name", declaredName)
	if declaredNamespace != "" {
		choice, _ = sjson.SetBytes(choice, "namespace", declaredNamespace)
	}
	return choice
}

func normalizeCodexLiteToolChoice(body []byte) []byte {
	convert := func(choice gjson.Result) []byte {
		switch choice.Get("type").String() {
		case "image_generation":
			return codexLiteFunctionReference(body, "image_gen", "imagegen")
		case "web_search", "web_search_preview", "web_search_preview_2025_03_11":
			return codexLiteFunctionReference(body, "web", "run")
		}
		return nil
	}
	choice := gjson.GetBytes(body, "tool_choice")
	if raw := convert(choice); raw != nil {
		body, _ = sjson.SetRawBytes(body, "tool_choice", raw)
	} else if choices := choice.Get("tools"); choices.IsArray() {
		for index, entry := range choices.Array() {
			if raw := convert(entry); raw != nil {
				body, _ = sjson.SetRawBytes(body, "tool_choice.tools."+strconv.Itoa(index), raw)
			}
		}
	}
	return body
}

// NormalizeCodexLiteCompatibilityTools ensures the enabled image declaration and
// converts existing hosted image/search tools for Lite. Search is never added unrequested.
// Client schemas are retained; tool execution remains the caller's responsibility.
func NormalizeCodexLiteCompatibilityTools(body []byte, injectImages bool) []byte {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return body
	}
	for _, path := range []string{"input", "tools"} {
		if value := gjson.GetBytes(body, path); value.Exists() && !value.IsArray() {
			return body
		}
	}
	images := injectImages || codexHasHostedTool(body, "image_generation")
	web := codexHasHostedTool(body, "web_search", "web_search_preview", "web_search_preview_2025_03_11")
	// Remove hosted declarations before adding their client-executed counterparts.
	transform := func(tools gjson.Result) ([]byte, bool) {
		if !tools.IsArray() {
			return nil, false
		}
		changed := false
		kept := make([]json.RawMessage, 0, len(tools.Array()))
		for _, tool := range tools.Array() {
			typ := tool.Get("type").String()
			if typ == "image_generation" && images || (typ == "web_search" || typ == "web_search_preview" || typ == "web_search_preview_2025_03_11") && web {
				changed = true
				continue
			}
			kept = append(kept, json.RawMessage(tool.Raw))
		}
		raw, _ := json.Marshal(kept)
		return raw, changed
	}
	body = transformCodexToolLists(body, transform)
	var additions []json.RawMessage
	for _, spec := range []struct {
		enabled              bool
		namespace, name, raw string
	}{
		{images, "image_gen", "imagegen", codexLiteImageNamespace}, {web, "web", "run", codexLiteWebNamespace},
	} {
		if !spec.enabled || codexDeclaredFunction(body, spec.namespace, spec.name) {
			continue
		}
		merged := false
		child := gjson.Parse(spec.raw).Get("tools.0")
		body = transformCodexToolLists(body, func(tools gjson.Result) ([]byte, bool) {
			if merged || !tools.IsArray() {
				return nil, false
			}
			for index, tool := range tools.Array() {
				if tool.Get("type").String() == "namespace" && tool.Get("name").String() == spec.namespace {
					raw, err := sjson.SetRaw(tools.Raw, strconv.Itoa(index)+".tools.-1", child.Raw)
					if err == nil {
						merged = true
						return []byte(raw), true
					}
				}
			}
			return nil, false
		})
		if !merged {
			additions = append(additions, json.RawMessage(spec.raw))
		}
	}
	// Move compatibility top-level declarations to a stable additional_tools item.
	top := gjson.GetBytes(body, "tools")
	if top.IsArray() {
		for _, tool := range top.Array() {
			additions = append(additions, json.RawMessage(tool.Raw))
		}
		body, _ = sjson.DeleteBytes(body, "tools")
	}
	if len(additions) > 0 {
		// Merge namespace children, keeping original declarations when names collide.
		additions = mergeCodexLiteNamespaces(additions)
		raw, _ := json.Marshal(additions)
		item := []byte(`{"type":"additional_tools","role":"developer"}`)
		item, _ = sjson.SetRawBytes(item, "tools", raw)
		item, _ = sjson.SetBytes(item, "id", codexLiteItemID(body, "at", raw))
		input := gjson.GetBytes(body, "input")
		items := []json.RawMessage{item}
		for _, value := range input.Array() {
			items = append(items, json.RawMessage(value.Raw))
		}
		rawInput, _ := json.Marshal(items)
		body, _ = sjson.SetRawBytes(body, "input", rawInput)
	}
	if instructions := gjson.GetBytes(body, "instructions"); instructions.Type == gjson.String {
		if instructions.String() != "" {
			item := []byte(`{"type":"message","role":"developer","content":[{"type":"input_text","text":""}]}`)
			item, _ = sjson.SetBytes(item, "content.0.text", instructions.String())
			item, _ = sjson.SetBytes(item, "id", codexLiteItemID(body, "msg", []byte(instructions.String())))
			items := []json.RawMessage{item}
			for _, value := range gjson.GetBytes(body, "input").Array() {
				items = append(items, json.RawMessage(value.Raw))
			}
			raw, _ := json.Marshal(items)
			body, _ = sjson.SetRawBytes(body, "input", raw)
		}
		body, _ = sjson.DeleteBytes(body, "instructions")
	}
	body = normalizeCodexLiteToolChoice(body)
	// Reverse the known compatibility names emitted by Chat Completions translation.
	for i, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() != "function_call" || item.Get("namespace").String() != "" {
			continue
		}
		for _, pair := range [][2]string{{"image_gen", "imagegen"}, {"web", "run"}} {
			name := item.Get("name").String()
			namespace, declaredName := codexLiteFunctionIdentity(body, pair[0], pair[1])
			if (name == pair[0]+"__"+pair[1] || name == pair[0]+"."+pair[1]) && namespace == pair[0] && declaredName == pair[1] {
				path := "input." + strconv.Itoa(i)
				body, _ = sjson.SetBytes(body, path+".namespace", pair[0])
				body, _ = sjson.SetBytes(body, path+".name", pair[1])
			}
		}
	}
	return body
}

func mergeCodexLiteNamespaces(tools []json.RawMessage) []json.RawMessage {
	var result []json.RawMessage
	indexes := map[string]int{}
	for _, raw := range tools {
		tool := gjson.ParseBytes(raw)
		if tool.Get("type").String() != "namespace" {
			result = append(result, raw)
			continue
		}
		name := tool.Get("name").String()
		index, exists := indexes[name]
		if !exists {
			indexes[name] = len(result)
			result = append(result, raw)
			continue
		}
		combined := result[index]
		seen := make(map[string]bool)
		for _, child := range gjson.GetBytes(combined, "tools").Array() {
			seen[child.Get("type").String()+"\x00"+child.Get("name").String()] = true
		}
		for _, child := range tool.Get("tools").Array() {
			key := child.Get("type").String() + "\x00" + child.Get("name").String()
			if seen[key] {
				continue
			}
			combined, _ = sjson.SetRawBytes(combined, "tools.-1", []byte(child.Raw))
			seen[key] = true
		}
		result[index] = combined
	}
	return result
}

func transformCodexToolLists(body []byte, transform func(gjson.Result) ([]byte, bool)) []byte {
	if raw, changed := transform(gjson.GetBytes(body, "tools")); changed {
		body, _ = sjson.SetRawBytes(body, "tools", raw)
	}
	for i, item := range gjson.GetBytes(body, "input").Array() {
		if item.Get("type").String() != "additional_tools" {
			continue
		}
		if raw, changed := transform(item.Get("tools")); changed {
			path := "input." + strconv.Itoa(i)
			body, _ = sjson.SetRawBytes(body, path+".tools", raw)
			if item.Get("id").Exists() {
				body, _ = sjson.SetBytes(body, path+".id", codexLiteItemID(body, "at", raw))
			}
		}
	}
	return body
}

// StripCodexImageTools removes declarations only, never function-call history or schemas.
func StripCodexImageTools(body []byte) []byte {
	var filter func(gjson.Result, string) ([]byte, bool)
	filter = func(tools gjson.Result, namespace string) ([]byte, bool) {
		if !tools.IsArray() {
			return nil, false
		}
		kept := make([]json.RawMessage, 0, len(tools.Array()))
		changed := false
		for _, tool := range tools.Array() {
			if CodexImageTool(tool, namespace) {
				changed = true
				continue
			}
			raw := []byte(tool.Raw)
			if tool.Get("type").String() == "namespace" {
				children, childChanged := filter(tool.Get("tools"), tool.Get("name").String())
				if childChanged {
					changed = true
					if string(children) == "[]" {
						continue
					}
					raw, _ = sjson.SetRawBytes(raw, "tools", children)
				}
			}
			kept = append(kept, raw)
		}
		raw, _ := json.Marshal(kept)
		return raw, changed
	}
	body = transformCodexToolLists(body, func(tools gjson.Result) ([]byte, bool) { return filter(tools, "") })
	choice := gjson.GetBytes(body, "tool_choice")
	if CodexImageTool(choice, "") || choice.Type == gjson.String && (choice.String() == "image_generation" || choice.String() == "image_gen.imagegen" || choice.String() == "image_gen__imagegen") {
		body, _ = sjson.DeleteBytes(body, "tool_choice")
	} else if raw, changed := filter(choice.Get("tools"), ""); changed {
		if string(raw) == "[]" {
			body, _ = sjson.DeleteBytes(body, "tool_choice")
		} else {
			body, _ = sjson.SetRawBytes(body, "tool_choice.tools", raw)
		}
	}
	return body
}
