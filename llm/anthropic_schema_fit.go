package llm

import (
	"fmt"
	"sort"
	"regexp"
	"strings"
)

// Anthropic's documented structured-output limits, checked before a call so
// the client picks one mode and never pays for a refusal. Sources (read
// 2026-10-16):
//   - https://platform.claude.com/docs/en/build-with-claude/structured-outputs
//     (supported keywords; additionalProperties must be false; at most 24
//     optional parameters and 16 union-typed parameters across a request;
//     no recursive or external $ref; enum of scalars only; regex without
//     backreferences, lookaround or word boundaries)
//   - https://platform.claude.com/docs/en/agents-and-tools/tool-use/define-tools
//     (forced tool_choice: Opus 5.5, Sonnet 5.5, Fable 5.1 and Mythos 5.1
//     answer 400 to type "tool" or "any")
const (
	anthropicMaxOptionalParameters = 24
	anthropicMaxUnionParameters    = 16
)

// anthropicStructuredKeywords are the schema keywords structured outputs
// accept, after anthropicSchema has dropped the numeric, string and array
// bounds the API refuses (the documented SDK behaviour moves them into
// descriptions; this client drops them and the caller's own check keeps
// them). "$defs" and "definitions" hold named schemas.
var anthropicStructuredKeywords = map[string]struct{}{
	"type": {}, "properties": {}, "required": {}, "additionalProperties": {}, "items": {},
	"enum": {}, "const": {}, "anyOf": {}, "allOf": {}, "$ref": {}, "$defs": {}, "definitions": {},
	"description": {}, "title": {}, "default": {}, "examples": {}, "format": {}, "minItems": {},
	"pattern": {}, "$schema": {}, "$id": {}, "$comment": {},
}

var anthropicStringFormats = map[string]struct{}{
	"date-time": {}, "time": {}, "date": {}, "duration": {}, "email": {}, "hostname": {},
	"uri": {}, "ipv4": {}, "ipv6": {}, "uuid": {},
}

var anthropicUnsupportedPattern = regexp.MustCompile(`\\[1-9]|\(\?[=!<]|\\[bB]`)

// anthropicNoForcedTool are the models the docs list as refusing a forced
// tool_choice; they get tool_choice "auto" with the one tool instead.
var anthropicNoForcedTool = []string{"claude-opus-5-5", "claude-sonnet-5-5", "claude-fable-5-1", "claude-mythos-5-1"}

func anthropicForcesTools(model string) bool {
	for _, prefix := range anthropicNoForcedTool {
		if strings.HasPrefix(model, prefix) {
			return false
		}
	}
	return true
}

// anthropicSchemaFit reports whether schema (as anthropicSchema sends it)
// fits structured outputs, and when it does not, the first documented limit
// it breaks.
func anthropicSchemaFit(schema map[string]any) (bool, string) {
	counts := &fitCounts{}
	if reason := fitNode(schema, "$", counts, 0); reason != "" {
		return false, reason
	}
	if counts.optional > anthropicMaxOptionalParameters {
		return false, fmt.Sprintf("optional_parameters: %d over the limit of %d", counts.optional, anthropicMaxOptionalParameters)
	}
	if counts.unions > anthropicMaxUnionParameters {
		return false, fmt.Sprintf("union_parameters: %d over the limit of %d", counts.unions, anthropicMaxUnionParameters)
	}
	return true, ""
}

type fitCounts struct{ optional, unions int }

func fitNode(node any, path string, counts *fitCounts, depth int) string {
	if depth > 64 {
		return "depth: " + path
	}
	schema, ok := node.(map[string]any)
	if !ok {
		return ""
	}
	for _, key := range sortedKeys(schema) {
		if _, ok := anthropicStructuredKeywords[key]; !ok {
			return "unsupported_keyword " + key + ": " + path
		}
	}
	if ref, ok := schema["$ref"].(string); ok && !strings.HasPrefix(ref, "#") {
		return "external_ref: " + path
	}
	if format, ok := schema["format"].(string); ok {
		if _, known := anthropicStringFormats[format]; !known {
			return "string_format " + format + ": " + path
		}
	}
	if pattern, ok := schema["pattern"].(string); ok && anthropicUnsupportedPattern.MatchString(pattern) {
		return "pattern: " + path
	}
	if values, ok := schema["enum"].([]any); ok {
		for _, value := range values {
			switch value.(type) {
			case map[string]any, []any:
				return "complex_enum: " + path
			}
		}
	}
	isObject := schema["type"] == "object" || schema["properties"] != nil
	if isObject {
		if additional, present := schema["additionalProperties"]; !present || additional != false {
			return "additional_properties_not_false: " + path
		}
	}
	required := map[string]bool{}
	if names, ok := schema["required"].([]any); ok {
		for _, name := range names {
			if text, ok := name.(string); ok {
				required[text] = true
			}
		}
	}
	if properties, ok := schema["properties"].(map[string]any); ok {
		for _, name := range sortedKeys(properties) {
			property := properties[name]
			propertyPath := path + "." + name
			child, ok := property.(map[string]any)
			if !ok {
				return "property_not_a_schema: " + propertyPath
			}
			if !typed(child) {
				return "property_without_type: " + propertyPath
			}
			if !required[name] {
				counts.optional++
			}
			if isUnion(child) {
				counts.unions++
			}
			if reason := fitNode(child, propertyPath, counts, depth+1); reason != "" {
				return reason
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if child, ok := schema[key].(map[string]any); ok {
			if reason := fitNode(child, path+"."+key, counts, depth+1); reason != "" {
				return reason
			}
		}
	}
	for _, key := range []string{"anyOf", "allOf"} {
		if list, ok := schema[key].([]any); ok {
			for i, child := range list {
				if reason := fitNode(child, fmt.Sprintf("%s.%s[%d]", path, key, i), counts, depth+1); reason != "" {
					return reason
				}
			}
		}
	}
	for _, key := range []string{"$defs", "definitions"} {
		if defs, ok := schema[key].(map[string]any); ok {
			for _, name := range sortedKeys(defs) {
				child := defs[name]
				if reason := fitNode(child, path+"."+key+"."+name, counts, depth+1); reason != "" {
					return reason
				}
			}
		}
	}
	return ""
}

func typed(schema map[string]any) bool {
	for _, key := range []string{"type", "anyOf", "allOf", "enum", "const", "$ref"} {
		if _, ok := schema[key]; ok {
			return true
		}
	}
	return false
}

func isUnion(schema map[string]any) bool {
	if _, ok := schema["anyOf"]; ok {
		return true
	}
	_, list := schema["type"].([]any)
	return list
}

// sortedKeys keeps the first reported limit the same on every call, so a
// caller can key a once-per-schema log on it.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
