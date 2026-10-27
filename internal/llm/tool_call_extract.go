package llm

import (
	"encoding/json"
	"regexp"
	"strconv"
	"strings"

	"gogen/internal/randhex"
)

var (
	// toolCallFunctionRegex matches <function>tool_name</function>
	toolCallFunctionRegex = regexp.MustCompile(`(?i)<function>\s*(\w+)\s*</function>`)

	// toolCallFunctionEqRegex matches <function=tool_name> (equals-sign format used by some models)
	toolCallFunctionEqRegex = regexp.MustCompile(`(?i)<function\s*=\s*(\w+)\s*>`)

	// toolCallFunctionAttrRegex matches <function name="tool_name"> or <function name='tool_name'>
	toolCallFunctionAttrRegex = regexp.MustCompile(`(?i)<function\s+name\s*=\s*["'](\w+)["']\s*>`)

	// toolCallInvokeRegex matches Anthropic-style <invoke name="tool_name">
	toolCallInvokeRegex = regexp.MustCompile(`(?i)<invoke\s+name\s*=\s*["'](\w+)["']\s*>`)

	// toolCallToolNameRegex matches <tool_name>name</tool_name>
	toolCallToolNameRegex = regexp.MustCompile(`(?i)<tool_name>\s*(\w+)\s*</tool_name>`)

	// toolCallParamEqRegex matches <parameter=name>value</parameter> (equals-sign format)
	toolCallParamEqRegex = regexp.MustCompile(`(?si)<parameter\s*=\s*(\w+)\s*>\s*(.*?)\s*</parameter>`)

	// toolCallParamAttrRegex matches <parameter name="name">value</parameter> (attribute format,
	// including Anthropic-style)
	toolCallParamAttrRegex = regexp.MustCompile(`(?si)<parameter\s+name\s*=\s*["'](\w+)["']\s*>\s*(.*?)\s*</parameter>`)

	// toolCallParametersRegex matches <parameters>...</parameters> or <parameter>...</parameter>
	toolCallParametersRegex = regexp.MustCompile(`(?i)<parameters>\s*(.*?)\s*</parameters>`)

	// toolCallJSONNameRegex finds occurrences of {"name": "tool_name" or {"name":"tool_name"
	toolCallJSONNameRegex = regexp.MustCompile(`(?i)\{"name"\s*:\s*["'](\w+)["']`)

	// toolCallBlockRegex matches <tool_call> ... </tool_call> blocks - using non-greedy but safe pattern
	toolCallBlockRegex = regexp.MustCompile(`(?s)<tool_call>\s*(.*?)\s*</tool_call>`)

	// toolInvokeBlockRegex matches full <invoke ...> ... </invoke> blocks (Anthropic-style)
	toolInvokeBlockRegex = regexp.MustCompile(`(?s)<invoke\s[^>]*>.*?</invoke>`)
)

// byteRange is a [start, end) byte offset pair used to track tag locations.
type byteRange struct{ start, end int }

// extractToolCallsFromText scans text for embedded tool call patterns and returns them as ToolCall objects.
func extractToolCallsFromText(text string) []ToolCall {
	return extractToolCallsFromTextMode(text, true)
}

// extractToolCallsFromTextMode is extractToolCallsFromText with control over
// the weak-signal bare-JSON scan. With allowBareJSON=false, explicit
// <tool_call>/<invoke> blocks are still extracted, but bare {"name": ...}
// objects in prose are never treated as calls. Reasoning segments are scanned
// in this mode: models routinely draft or quote JSON examples there, and the
// old behavior mis-executed them as phantom tool calls.
func extractToolCallsFromTextMode(text string, allowBareJSON bool) []ToolCall {
	var toolCalls []ToolCall

	// First, try to find <tool_call> ... </tool_call> blocks.
	// A single FindAllStringSubmatchIndex pass yields both the byte range of
	// each block (for the <invoke> skip set) and the captured content.
	blockLocs := toolCallBlockRegex.FindAllStringSubmatchIndex(text, -1)

	// Track <tool_call> block byte ranges so we can skip <invoke>
	// blocks that are already nested inside them.
	toolCallRanges := make([]byteRange, 0, len(blockLocs))
	for _, loc := range blockLocs {
		toolCallRanges = append(toolCallRanges, byteRange{
			start: loc[0],
			end:   loc[1],
		})
		if len(loc) >= 4 && loc[2] >= 0 && loc[3] >= loc[2] {
			blockContent := text[loc[2]:loc[3]]
			calls := extractToolCallsFromBlock(blockContent)
			toolCalls = append(toolCalls, calls...)
		}
	}

	// Also try <invoke> blocks (Anthropic-style, may appear outside <tool_call>)
	invokeLocs := toolInvokeBlockRegex.FindAllStringIndex(text, -1)
	for _, loc := range invokeLocs {
		if isInsideAnyByteRange(loc[0], loc[1], toolCallRanges) {
			continue // already extracted as part of a <tool_call> block
		}
		fullMatch := text[loc[0]:loc[1]]
		calls := extractToolCallsFromBlock(fullMatch)
		toolCalls = append(toolCalls, calls...)
	}

	if allowBareJSON {
		// Always scan for bare JSON tool-call objects ({"name": ...}) so a reply
		// that MIXES XML-wrapped calls with plain JSON objects does not silently
		// drop the JSON ones (the previous len(toolCalls)==0 gate meant a single
		// <tool_call>/<invoke> block suppressed every bare JSON call after it).
		// Objects that fall inside an already-parsed <tool_call>/<invoke> block
		// are skipped — the block path extracted them, and a text-level re-parse
		// would only duplicate them.
		//
		// A bare object is accepted only when it carries an arguments/input
		// container — i.e. it is shaped like a genuine call. Prose JSON
		// ({"name": "service", "version": 2}, name-only objects, flattened or
		// array-element shapes) is structurally indistinguishable from a
		// call otherwise and used to be mis-executed as a phantom tool call.
		// Flattened/name-only JSON is still recoverable inside explicit
		// <tool_call>/<invoke> blocks, where the intent is unambiguous.
		skipRanges := make([]byteRange, 0, len(toolCallRanges)+len(invokeLocs))
		skipRanges = append(skipRanges, toolCallRanges...)
		for _, loc := range invokeLocs {
			skipRanges = append(skipRanges, byteRange{start: loc[0], end: loc[1]})
		}
		matches := toolCallJSONNameRegex.FindAllStringIndex(text, -1)
		seenEnd := make(map[int]struct{})
		for _, match := range matches {
			// Locate the outermost '{' of the JSON object
			objStart := findJSONObjStart(text, match[0])
			if objStart >= 0 {
				jsonStr, endIdx := extractJSONObject(text, objStart)
				if jsonStr != "" && endIdx > objStart {
					if isInsideAnyByteRange(objStart, endIdx, skipRanges) {
						continue
					}
					if _, ok := seenEnd[endIdx]; ok {
						continue
					}
					seenEnd[endIdx] = struct{}{}
					// An object that is an array element is prose (a list of
					// call-shaped examples), not a standalone call.
					if objStart > 0 && text[objStart-1] == '[' {
						continue
					}
					var obj map[string]any
					if err := json.Unmarshal([]byte(jsonStr), &obj); err != nil {
						continue
					}
					if !isToolCallObject(obj) {
						continue
					}
					toolCalls = append(toolCalls, parseToolCallFromObject(obj)...)
				}
			}
		}
	}

	renumberExtractedToolCalls(toolCalls)
	return toolCalls
}

// renumberExtractedToolCalls assigns unique, dense Index and ID values across
// a combined set of extracted calls. extractToolCallsFromBlock numbers each
// block's calls from its own local counter, so separate <tool_call>/<invoke>
// blocks would otherwise collide on "tc_extracted_0" (and Index 0), and a
// caller combining content and reasoning scans would too. Duplicate tool-call
// IDs violate the one-to-one tool-call/result protocol the agent relies on
// when it appends tool results keyed by ID. Every call produced by the
// extractors carries a synthetic ID (the block extractor always assigns one),
// so renumbering is safe.
func renumberExtractedToolCalls(toolCalls []ToolCall) {
	for i := range toolCalls {
		toolCalls[i].Index = i
		// Unique per batch, not per index: a fixed "tc_extracted_<i>" collided
		// across rounds, so history carried duplicate tool-call IDs — the
		// tool-result roster then matched the wrong results ("first result
		// wins") and strict providers reject the request outright.
		toolCalls[i].ID = randhex.ID(8, "tc_")
	}
}

// isToolCallObject reports whether a decoded JSON object is shaped like a
// genuine tool call: a resolvable name plus an arguments/input container.
// Name-only objects, flattened shapes, and prose JSON such as
// {"name": "service", "version": 2} are rejected, so they are never
// mis-executed as phantom tool calls.
func isToolCallObject(obj map[string]any) bool {
	if toolCallNameFromObject(obj) == "" {
		return false
	}
	return obj["arguments"] != nil || obj["input"] != nil
}

// isInsideAnyByteRange reports whether byte range [start, end) falls entirely
// within any of the given ranges.
func isInsideAnyByteRange(start, end int, ranges []byteRange) bool {
	for _, r := range ranges {
		if start >= r.start && end <= r.end {
			return true
		}
	}
	return false
}

// findJSONObjStart scans backwards from idx to find the outermost '{' that
// begins a JSON object, respecting string boundaries. Returns -1 if no
// suitable '{' is found before hitting an unrelated quote character.
func findJSONObjStart(text string, idx int) int {
	for i := idx; i >= 0; i-- {
		switch text[i] {
		case '{':
			return i
		case '"':
			// We hit a string boundary without finding '{' — can't
			// reliably determine the object start from here.
			return -1
		}
	}
	return -1
}

// extractToolCallsFromBlock extracts tool calls from a <tool_call> ... </tool_call> block content
func extractToolCallsFromBlock(blockContent string) []ToolCall {
	var toolCalls []ToolCall

	// Strategy 1: Try to find JSON tool call objects inside the block
	// (e.g. <tool_call>{"name": "...", "arguments": {...}}</tool_call>)
	// FindAllStringIndex so each match can locate its own object; scanning from
	// byte 0 on every match would re-parse the first object repeatedly.
	jsonMatches := toolCallJSONNameRegex.FindAllStringIndex(blockContent, -1)
	if len(jsonMatches) > 0 {
		seenEnd := make(map[int]struct{})
		for _, loc := range jsonMatches {
			startIdx := loc[0]
			objStart := findJSONObjStart(blockContent, startIdx)
			if objStart < 0 {
				continue
			}
			jsonStr, endIdx := extractJSONObject(blockContent, objStart)
			if jsonStr == "" || endIdx <= objStart {
				continue
			}
			if _, ok := seenEnd[endIdx]; ok {
				continue
			}
			seenEnd[endIdx] = struct{}{}
			calls := parseToolCallFromJSONString(jsonStr)
			for i := range calls {
				calls[i].Index = len(toolCalls)
				calls[i].ID = "tc_extracted_" + strconv.Itoa(len(toolCalls))
			}
			toolCalls = append(toolCalls, calls...)
		}
		if len(toolCalls) > 0 {
			return toolCalls
		}
	}

	// Strategy 2: Try XML-based formats
	toolName, argsMap, argsStr := extractXMLToolCall(blockContent)
	if toolName != "" {
		toolCalls = append(toolCalls, ToolCall{
			Index:   len(toolCalls),
			ID:      "tc_extracted_" + strconv.Itoa(len(toolCalls)),
			Name:    toolName,
			Args:    argsMap,
			ArgsStr: argsStr,
		})
	}

	return toolCalls
}

// extractXMLToolCall tries multiple XML-based tool call formats and returns
// the tool name, parsed args, and raw args string. Returns empty name if no
// format matches.
func extractXMLToolCall(blockContent string) (string, map[string]any, string) {
	// Try each function-name extraction pattern (ordered most-specific first)
	funcPatterns := []struct {
		re   *regexp.Regexp
		name string // description for debugging
	}{
		{toolCallFunctionAttrRegex, "function name=attr"},
		{toolCallFunctionEqRegex, "function=name"},
		{toolCallFunctionRegex, "function>name<"},
		{toolCallInvokeRegex, "invoke name=attr"},
		{toolCallToolNameRegex, "tool_name"},
	}

	for _, fp := range funcPatterns {
		funcMatches := fp.re.FindAllStringSubmatch(blockContent, -1)
		for _, match := range funcMatches {
			if len(match) >= 2 {
				toolName := match[1]

				// Try <parameters>JSON</parameters> first (most common)
				if paramMatches := toolCallParametersRegex.FindAllStringSubmatch(blockContent, -1); len(paramMatches) > 0 {
					paramContent := paramMatches[0][1]
					argsMap, argsStr := parseParamContent(paramContent)
					return toolName, argsMap, argsStr
				}

				// Try individual <parameter=name>value</parameter> (equals-sign format)
				if paramEqMatches := toolCallParamEqRegex.FindAllStringSubmatch(blockContent, -1); len(paramEqMatches) > 0 {
					argsMap := make(map[string]any)
					for _, pm := range paramEqMatches {
						if len(pm) >= 3 {
							pName := pm[1]
							pValue := strings.TrimSpace(pm[2])
							argsMap[pName] = parseParamValue(pValue)
						}
					}
					argsJSON, _ := json.Marshal(argsMap)
					return toolName, argsMap, string(argsJSON)
				}

				// Try individual <parameter name="name">value</parameter> (attribute format)
				if paramAttrMatches := toolCallParamAttrRegex.FindAllStringSubmatch(blockContent, -1); len(paramAttrMatches) > 0 {
					argsMap := make(map[string]any)
					for _, pm := range paramAttrMatches {
						if len(pm) >= 3 {
							pName := pm[1]
							pValue := strings.TrimSpace(pm[2])
							argsMap[pName] = parseParamValue(pValue)
						}
					}
					argsJSON, _ := json.Marshal(argsMap)
					return toolName, argsMap, string(argsJSON)
				}

				// Try JSON arguments in the block content
				// Use brace-counting to handle nested JSON objects,
				// unlike the flat regex which only matches \{[^{}]*\}.
				argsJSON := extractJSONArgValue(blockContent, "arguments")
				if argsJSON == "" {
					argsJSON = extractJSONArgValue(blockContent, "input")
				}
				if argsJSON != "" {
					var parsedArgs map[string]any
					if err := json.Unmarshal([]byte(argsJSON), &parsedArgs); err == nil {
						return toolName, parsedArgs, argsJSON
					}
					// Fallback: wrap as input
					return toolName, map[string]any{"input": argsJSON}, argsJSON
				}

				// No parameters found — return empty args
				return toolName, map[string]any{}, "{}"
			}
		}
	}

	return "", nil, ""
}

// extractJSONArgValue finds a JSON object value for the given key in blockContent.
// It scans for `"key":` then uses brace-counting (extractJSONObject) to pull out
// the full JSON value, supporting nested objects. Returns empty string if not found.
func extractJSONArgValue(blockContent, key string) string {
	// Find the key in the content
	search := `"` + key + `"`
	idx := strings.Index(blockContent, search)
	if idx < 0 {
		return ""
	}
	// Advance past the key and colon
	rest := blockContent[idx+len(search):]
	colonIdx := strings.Index(rest, ":")
	if colonIdx < 0 {
		return ""
	}
	rest = rest[colonIdx+1:]
	// Skip whitespace
	rest = strings.TrimLeft(rest, " \t\r\n")
	if len(rest) == 0 || rest[0] != '{' {
		return ""
	}
	jsonStr, _ := extractJSONObject(rest, 0)
	if jsonStr == "" {
		return ""
	}
	return jsonStr
}

// parseParamContent parses the content of a <parameters> tag, trying JSON first.
func parseParamContent(paramContent string) (map[string]any, string) {
	// Try to parse as JSON
	var parsedArgs map[string]any
	if err := json.Unmarshal([]byte(paramContent), &parsedArgs); err == nil {
		return parsedArgs, paramContent
	}
	// Try to extract JSON object from the parameter content
	jsonStr, _ := extractJSONObject(paramContent, 0)
	if jsonStr != "" {
		var inner map[string]any
		if err := json.Unmarshal([]byte(jsonStr), &inner); err == nil {
			return inner, jsonStr
		}
	}
	return map[string]any{"input": paramContent}, paramContent
}

// parseParamValue tries to interpret a parameter value as a typed Go value.
func parseParamValue(v string) any {
	switch strings.ToLower(v) {
	case "true":
		return true
	case "false":
		return false
	case "null", "none", "nil":
		return nil
	}
	// Try integer
	if iv, err := strconv.ParseInt(v, 10, 64); err == nil {
		if strconv.FormatInt(iv, 10) == v {
			if iv >= -(1<<53) && iv <= 1<<53 {
				return float64(iv)
			}
			// Beyond float64's exact integer range: return int64 so the
			// value round-trips exactly (float64 would silently round to a
			// neighboring value, e.g. 9007199254740993 -> 9007199254740992).
			// Consumers accept both shapes (intArgOptional handles int64).
			return iv
		}
	}
	// Try float
	if fv, err := strconv.ParseFloat(v, 64); err == nil {
		return fv
	}
	return v
}

// extractJSONObject extracts a JSON object starting at src[start:]
// It returns the JSON string and the end index (exclusive)
func extractJSONObject(src string, start int) (string, int) {
	if start >= len(src) || src[start] != '{' {
		return "", -1
	}
	depth := 0
	i := start
	for i < len(src) {
		c := src[i]
		if c == '{' {
			depth++
			i++
		} else if c == '}' {
			depth--
			if depth == 0 {
				return src[start : i+1], i + 1
			}
			i++
		} else if c == '"' {
			// skip string
			i++
			for i < len(src) {
				if src[i] == '\\' {
					i += 2
				} else if src[i] == '"' {
					i++ // advance past the closing quote
					break
				} else {
					i++
				}
			}
		} else {
			i++
		}
	}
	return "", -1
}

func parseToolCallFromJSONString(jsonStr string) []ToolCall {
	// Try to unmarshal as a map
	var obj map[string]any
	if err := json.Unmarshal([]byte(jsonStr), &obj); err != nil {
		return nil
	}
	return parseToolCallFromObject(obj)
}

// parseToolCallFromObject builds a ToolCall from an already-decoded JSON
// object. Note: Index and ID are assigned by the caller
// (extractToolCallsFromBlock / extractToolCallsFromTextMode) to ensure they
// are unique within the full result set.
func parseToolCallFromObject(obj map[string]any) []ToolCall {
	name := toolCallNameFromObject(obj)
	if name == "" {
		return nil
	}
	argsMap, argsStr := normalizeToolCallArguments(obj)
	return []ToolCall{{Name: name, Args: argsMap, ArgsStr: argsStr}}
}

// toolCallNameFromObject extracts the tool name from a decoded JSON object:
// the "name" field, or the "function" field when it is a string or an object
// with its own "name". Returns "" when no usable name is present.
func toolCallNameFromObject(obj map[string]any) string {
	if n, ok := obj["name"].(string); ok && n != "" {
		return n
	}
	if n, ok := obj["function"].(string); ok && n != "" {
		return n
	}
	if n, ok := obj["function"].(map[string]any); ok {
		if nName, ok := n["name"].(string); ok && nName != "" {
			return nName
		}
	}
	return ""
}

// normalizeToolCallArguments resolves the arguments of a decoded tool-call
// object into the Args map and canonical ArgsStr. The "arguments" field wins;
// when absent, "input" (or the first non-metadata key) is used, mirroring the
// recovery shapes models emit.
func normalizeToolCallArguments(obj map[string]any) (map[string]any, string) {
	arguments := obj["arguments"]
	if arguments == nil {
		// check if 'input' or the whole obj is the args
		if _, ok := obj["input"]; ok {
			arguments = obj["input"]
		} else {
			// try to find any other key that is an object or string
			for k, v := range obj {
				if k == "name" || k == "function" || k == "id" || k == "type" {
					continue
				}
				arguments = v
				break
			}
		}
	}

	argsMap := make(map[string]any)
	argsStr := ""

	if argsObj, ok := arguments.(map[string]any); ok {
		argsMap = argsObj
		argsJSON, err := json.Marshal(argsObj)
		if err == nil {
			argsStr = string(argsJSON)
		}
	} else if argsStrVal, ok := arguments.(string); ok {
		// try to parse string as JSON (same recovery as the streaming
		// accumulator's parseToolCallArgs); fall back to wrapping as input
		argsStr = argsStrVal
		if parsedArgs, err := parseToolCallArgs(argsStrVal); err == nil && strings.TrimSpace(argsStrVal) != "" {
			argsMap = parsedArgs
		} else {
			argsMap = map[string]any{"input": argsStrVal}
		}
	} else if arguments != nil {
		// fallback: marshal the arguments
		argsJSON, err := json.Marshal(arguments)
		if err == nil {
			argsStr = string(argsJSON)
			var parsedArgs map[string]any
			if json.Unmarshal(argsJSON, &parsedArgs) == nil {
				argsMap = parsedArgs
			} else {
				argsMap = map[string]any{"input": arguments}
			}
		} else {
			argsMap = map[string]any{"input": arguments}
		}
	}
	return argsMap, argsStr
}
