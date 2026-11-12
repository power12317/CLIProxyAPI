package basispoints

// collectCatalog validates each declaration batch independently. Later history
// batches update the active catalog without changing earlier call contracts.
func (b *Bridge) collectCatalog(body object) ([]tool, map[string]tool, map[int][]tool, error) {
	var catalog []tool
	positions := make(map[string]int)
	initial := make(map[string]tool)
	updates := make(map[int][]tool)
	merge := func(value any) ([]tool, error) {
		batch, native, err := collectToolBatch(value)
		if err != nil {
			return nil, err
		}
		for _, entry := range batch {
			key := toolKey(entry)
			if old, exists := b.tools[key]; exists && (old.Name != entry.Name || old.Namespace != entry.Namespace) {
				return nil, failure(400, "invalid_tool", "ambiguous client tool namespace: "+key)
			}
			b.tools[key] = entry
			if index, exists := positions[key]; exists {
				catalog[index] = entry
			} else {
				positions[key] = len(catalog)
				catalog = append(catalog, entry)
			}
		}
		for kind, entries := range native {
			b.nativeTools[kind] = entries
		}
		return batch, nil
	}
	top, err := merge(body["tools"])
	if err != nil {
		return nil, nil, nil, err
	}
	for _, entry := range top {
		initial[toolKey(entry)] = entry
	}
	if items, ok := body["input"].([]any); ok {
		for i, value := range items {
			item, _ := value.(object)
			if item["type"] == "additional_tools" || item["type"] == "tool_search_output" && item["execution"] == "client" {
				batch, err := merge(item["tools"])
				if err != nil {
					return nil, nil, nil, err
				}
				updates[i] = batch
			}
		}
	}
	return catalog, initial, updates, nil
}

func collectToolBatch(value any) ([]tool, map[string][]object, error) {
	var batch []tool
	seen := make(map[string]tool)
	native := make(map[string][]object)
	var collect func(any, string) error
	collect = func(value any, namespace string) error {
		if value == nil {
			return nil
		}
		entries, ok := value.([]any)
		if !ok {
			return failure(400, "invalid_tool", "tool declarations must be arrays")
		}
		for _, value := range entries {
			item, ok := value.(object)
			if !ok {
				return failure(400, "invalid_tool", "tool definitions must be objects")
			}
			kind, name := stringValue(item["type"]), stringValue(item["name"])
			if kind == "namespace" {
				if name == "" {
					return failure(400, "invalid_tool", "tool namespace is empty")
				}
				if namespace != "" {
					name = namespace + "." + name
				}
				if err := collect(item["tools"], name); err != nil {
					return err
				}
				continue
			}
			if kind == "tool_search" && item["execution"] == "client" {
				name = "tool_search"
			} else if nativeToolKind(kind) {
				native[kind] = append(native[kind], clone(item))
				continue
			} else if kind != "function" && kind != "custom" {
				return failure(400, "unsupported_tool", "Basispoints relay supports function and custom client tools")
			}
			key := name
			if namespace != "" {
				key = namespace + "." + name
			}
			if name == "" || nativeName(key) {
				return failure(400, "invalid_tool", "client tool name is empty or reserved")
			}
			parameters := item["parameters"]
			if parameters == nil {
				parameters = item["inputSchema"]
			}
			if parameters == nil {
				parameters = item["input_schema"]
			}
			entry := tool{Name: name, Namespace: namespace, Type: kind, Description: stringValue(item["description"]), Parameters: parameters, Format: item["format"], Strict: item["strict"]}
			if old, exists := seen[key]; exists {
				old.Description = entry.Description
				if digest(old) != digest(entry) {
					return failure(400, "invalid_tool", "conflicting client tool declaration: "+key)
				}
				continue
			}
			seen[key] = entry
			batch = append(batch, entry)
		}
		return nil
	}
	err := collect(value, "")
	return batch, native, err
}
