package tui

import (
	"fmt"
	"slices"
	"strings"
	"unicode/utf8"

	"gogen/internal/llm"
	"gogen/internal/textutil"
)

// renderMessages converts a slice of LLM messages into styled display lines
// suitable for the chat viewport. All messages are rendered (no truncation;
// the viewport handles scrolling).
func renderMessages(messages []llm.Message, workingDir string, modelName string, mode string, verbose bool) []string {
	var lines []string
	var tcMap map[string]llm.ToolCall
	tcMapNeeded := false

	// Header
	if modelName != "" {
		header := fmt.Sprintf("GoGen — %s (%s)", workingDir, mode)
		lines = append(lines, DimStyle.Render(header))
		lines = append(lines, "")
	}

	if len(messages) == 0 {
		return lines
	}

	// Build tool-call ID → ToolCall map so tool results show names and diffs.
	for _, msg := range messages {
		if msg.Role == "assistant" && len(msg.ToolCalls) > 0 {
			tcMapNeeded = true
			break
		}
	}
	if tcMapNeeded {
		tcMap = make(map[string]llm.ToolCall)
		for _, msg := range messages {
			if msg.Role == "assistant" {
				for _, tc := range msg.ToolCalls {
					tcMap[tc.ID] = tc
				}
			}
		}
	}

	for _, msg := range messages {
		switch msg.Role {
		case "user":
			if msg.Content != "" {
				label := UserStyle.Render(userLabel)
				lines = append(lines, label+" "+msg.Content)
			}
		case "assistant":
			// Always render thinking block when reasoning is present.
			if msg.Reasoning != "" {
				lines = append(lines, renderStyledBlock(ThinkingTagStyle, "<thinking>"+msg.Reasoning+"</thinking>"))
			}
			switch {
			case msg.Content != "":
				label := AssistantStyle.Render(assistantLabel)
				lines = append(lines, label+" "+msg.Content)
			case msg.Refusal != "":
				label := AssistantStyle.Render(assistantLabel)
				lines = append(lines, label+" "+msg.Refusal)
			}
			for _, tc := range msg.ToolCalls {
				prefix := ToolCallStyle.Render("  →")
				argStr := formatToolArgs(tc.Args)
				if argStr == "" {
					lines = append(lines, prefix+" "+tc.Name)
				} else {
					lines = append(lines, prefix+" "+tc.Name+" "+ToolCallArgsStyle.Render(argStr))
				}
			}
		case "tool":
			if msg.Content != "" {
				lines = append(lines, renderToolResult(msg, tcMap, verbose)...)
			}
		}
	}

	return lines
}

// toolResultParams is the input of the ONE tool-result renderer. The live
// stream (handleStreamToolResult) and a restored transcript
// (renderToolResult) both go through toolResultLines, so a result cannot look
// different depending on whether the user watched it stream or scrolled back
// to it — they had drifted on verbose mode (ignored when restoring), on
// patch_file without a diff, and on how success is detected.
type toolResultParams struct {
	name    string
	result  string
	success bool
	// diff is a patch_file diff to render as a block. The live path passes ""
	// once the diff was already rendered while the arguments streamed (its
	// border was closed there, so re-rendering would duplicate it); a restored
	// transcript passes the diff it found in the tool call's arguments.
	diff    string
	verbose bool
}

// toolResultLines renders a finished tool call: the status line, then either
// a diff block, the verbose body, or the one-line summary.
func toolResultLines(p toolResultParams) []string {
	lines := []string{toolResultStatusLine(p.name, p.success)}
	if p.name == "show_diff" && isDiffContent(p.result) {
		return append(lines, diffBlock(p.result)...)
	}
	if p.name == "patch_file" && p.diff != "" {
		return append(lines, diffBlock(p.diff)...)
	}
	// patch_file with no diff to show (already streamed, or never captured)
	// falls through to the same body/summary choice every other tool gets, so
	// such a result is never just a bare status line.
	if p.verbose {
		for _, line := range strings.Split(p.result, "\n") {
			lines = append(lines, ToolResultBodyStyle.Render("  │ "+line))
		}
		return lines
	}
	return append(lines, DimStyle.Render("  "+summarizeResult(p.result, p.success)))
}

// toolResultSuccess reports whether a PERSISTED tool result was a success.
// A restored transcript has no success flag (the message is plain text), so
// this is the only signal available: the agent prefixes failures with
// "Error:" (formatToolError). The live path knows better and passes its
// explicit bool; the heuristic is deliberately confined here, where a future
// persisted flag would replace it in one place.
func toolResultSuccess(content string) bool {
	return !strings.HasPrefix(strings.TrimSpace(content), "Error:")
}

// renderToolResult renders one tool result from the conversation history by
// resolving its tool call (name, and the patch_file diff) and delegating to
// the shared renderer.
func renderToolResult(msg llm.Message, tcMap map[string]llm.ToolCall, verbose bool) []string {
	toolName := ""
	diff := ""
	if tc, ok := tcMap[msg.ToolCallID]; ok {
		toolName = tc.Name
		if toolName == "patch_file" {
			if d, ok := tc.Args["diff"].(string); ok {
				diff = d
			}
		}
	}
	return toolResultLines(toolResultParams{
		name:    toolName,
		result:  msg.Content,
		success: toolResultSuccess(msg.Content),
		diff:    diff,
		verbose: verbose,
	})
}

// formatArgsMap renders tool-call arguments as "key=value" pairs joined by
// spaces, truncating values longer than maxLen (in runes) with an ellipsis.
// skip, when non-nil, omits keys for which it returns true. When no key
// survives, returns "".
func formatArgsMap(args map[string]any, maxLen int, skip func(string) bool) string {
	if len(args) == 0 {
		return ""
	}
	// Sorted keys: map iteration order is random, so the same tool call
	// rendered a different string on every rebuild. That reshuffled the
	// arguments in the transcript (breaking prompt-cache stability) and made
	// the final line differ from the streamed one.
	keys := make([]string, 0, len(args))
	for k := range args {
		if skip != nil && skip(k) {
			continue
		}
		keys = append(keys, k)
	}
	slices.Sort(keys)
	var parts []string
	for _, k := range keys {
		val := fmt.Sprintf("%v", args[k])
		// maxLen counts RUNES (truncateRunes); comparing bytes made a CJK value
		// get an ellipsis without actually being shortened.
		if utf8.RuneCountInString(val) > maxLen {
			val = textutil.TruncateRunes(val, maxLen-3) + "..."
		}
		parts = append(parts, fmt.Sprintf("%s=%q", k, val))
	}
	return strings.Join(parts, " ")
}

// formatToolArgs formats tool call arguments for display.
func formatToolArgs(args map[string]any) string {
	return formatArgsMap(args, 80, nil)
}

// formatArgsCompact renders inline JSON tool-call args like formatToolArgs,
// skipping the diff key; values longer than maxLen are truncated. When no
// keys survive, returns "". Takes a byte slice so streaming callers can pass
// the append-grown args buffer without an O(N) string materialization.
func formatArgsCompact(rawJSON []byte, maxLen int) string {
	args, err := parseInlineJSONArgs(rawJSON)
	if err != nil || len(args) == 0 {
		return ""
	}
	return formatArgsMap(args, maxLen, func(k string) bool { return k == "diff" })
}
