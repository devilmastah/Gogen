package agent

import (
	"gogen/internal/llm"
)

// ToolDef is the single registration record for a builtin tool: its LLM-facing
// schema and the handler that executes it. builtinToolDefs is the one source of
// truth — BuiltinTools and BuiltinToolHandlers are derived views of it, so a
// tool's schema and handler can never drift apart.
type ToolDef struct {
	Definition llm.Tool
	Handler    ToolHandler

	// ReadOnly marks handlers safe to execute concurrently within a single
	// turn: no workspace mutation, no shell execution, and no session-state
	// mutation performed inline. A ReadOnly handler may still affect the
	// session indirectly — read_image attaches images to the transcript —
	// but only through a deferred, model-ordered drain (the image sink)
	// that runs after the tool batch, never from inside the handler, so
	// concurrent execution stays safe. ReadOnly implies PlanAllowed.
	ReadOnly bool

	// PlanAllowed marks the tool as available in plan mode. Read-only tools
	// set both flags; the few session-local mutations allowed in plan mode
	// (todo, session_rename, context_pin_last) set PlanAllowed without
	// ReadOnly.
	PlanAllowed bool

	// MutatesFS marks tools that write to the working tree. The server wraps
	// these handlers with the workspace fsMu so editor saves and agent file
	// mutations stay serialized. MutatesFS tools are never ReadOnly and
	// never PlanAllowed.
	MutatesFS bool
}

// toolProp creates a property definition for a tool parameter.
func toolProp(typ, desc string) map[string]any {
	return map[string]any{
		"type":        typ,
		"description": desc,
	}
}

// toolPropArray creates an array property definition for a tool parameter.
func toolPropArray(itemType, desc string) map[string]any {
	return map[string]any{
		"type": "array",
		"items": map[string]any{
			"type": itemType,
		},
		"description": desc,
	}
}

// toolPropEnum creates an enum-constrained property definition. Enum values
// keep action/kind-style parameters honest: the provider sees the allowed
// values up front, so invalid ones fail argument validation instead of
// round-tripping through the handler. An empty description omits the key
// entirely — the enum array and the tool description already carry the values.
func toolPropEnum(typ string, values []string, desc string) map[string]any {
	prop := map[string]any{
		"type": typ,
		"enum": values,
	}
	if desc != "" {
		prop["description"] = desc
	}
	return prop
}

// toolSchema creates a tool parameter schema with optional required fields.
func toolSchema(props map[string]any, required ...string) map[string]any {
	schema := map[string]any{
		"type":       "object",
		"properties": props,
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	return schema
}

// toolDef creates a complete tool definition.
func toolDef(name, desc string, params map[string]any) llm.Tool {
	return llm.Tool{
		Name:        name,
		Description: desc,
		Parameters:  params,
	}
}

// builtinToolDefs is the single registration table for builtin tools. Order is
// significant: it is the order tools are sent to the LLM provider, and provider
// prompt caches are keyed on the serialized tool list. Keep new tools appended
// at the end; do not reorder existing entries.
var builtinToolDefs = []ToolDef{
	{
		Definition: toolDef("list_files", "List directory contents; recursive=true walks the tree (max 500 entries). Directories suffixed with /.",
			toolSchema(map[string]any{
				"path":         toolProp("string", "Directory path"),
				"recursive":    toolProp("boolean", "Walk tree recursively (max 500)"),
				"tracked_only": toolProp("boolean", "Only git-tracked files"),
			}, "path")),
		Handler:     handleListFiles,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("repo_overview", "Summarize repo layout: top-level directories, file counts, root files.",
			toolSchema(map[string]any{})),
		Handler:     handleRepoOverview,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("glob", "Find files by glob pattern (e.g. *.go, **/*.md); dotfiles match, hidden dirs are skipped unless passed as path.",
			toolSchema(map[string]any{
				"pattern":      toolProp("string", "Glob pattern"),
				"path":         toolProp("string", "Subdirectory"),
				"tracked_only": toolProp("boolean", "Only git-tracked files"),
			}, "pattern")),
		Handler:     handleGlobFiles,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("read_file", "Read file content; offset/limit for ranges, search for regex jump.",
			toolSchema(map[string]any{
				"path":         toolProp("string", "File path"),
				"offset":       toolProp("integer", "Start line; with search: context lines before the match (default 10)"),
				"limit":        toolProp("integer", "Max lines (default all, max 10000); with search: caps total lines in the match window"),
				"search":       toolProp("string", "Regex to jump to (returns a window of context lines around the match)"),
				"line_numbers": toolProp("boolean", "Prefix lines with numbers"),
			}, "path")),
		Handler:     handleReadFile,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("read_files", "Read multiple files (max 20, 512KB). Output: === path === headers.",
			toolSchema(map[string]any{
				"paths": toolPropArray("string", "File paths"),
			}, "paths")),
		Handler:     handleReadFiles,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("list_definitions", "List functions/types in a file with line numbers (AST or text outline).",
			toolSchema(map[string]any{
				"path": toolProp("string", "Source file path"),
			}, "path")),
		Handler:     handleListDefinitions,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("write_file", "Create a NEW file only (parent dirs ok); refuses existing paths — edit with patch_file/replace_in_file.",
			toolSchema(map[string]any{
				"path":    toolProp("string", "File path"),
				"content": toolProp("string", "Content"),
			}, "path", "content")),
		Handler:   handleWriteFile,
		MutatesFS: true,
	},
	{
		Definition: toolDef("execute_command", "Run a shell command (destructive patterns blocked).",
			toolSchema(map[string]any{
				"command":    toolProp("string", "Command"),
				"background": toolProp("boolean", "Run in the background; poll/cancel via background_job (default false)"),
			}, "command")),
		Handler: handleExecuteCommand,
	},
	{
		Definition: toolDef("replace_in_file", "Default edit tool: exact search/replace in an existing file. Copy the search block verbatim from read_file output; add surrounding context to disambiguate.",
			toolSchema(map[string]any{
				"path":        toolProp("string", "File path"),
				"search":      toolProp("string", "Exact string to find (must match exactly once unless replace_all=true)"),
				"replace":     toolProp("string", "Replacement"),
				"replace_all": toolProp("boolean", "Replace all occurrences (default false)"),
			}, "path", "search", "replace")),
		Handler:   handleReplaceInFile,
		MutatesFS: true,
	},
	{
		Definition: toolDef("delete", "Delete a file or empty directory (requires approval); non-empty directories are refused.",
			toolSchema(map[string]any{
				"path": toolProp("string", "File path"),
			}, "path")),
		Handler:   handleDelete,
		MutatesFS: true,
	},
	{
		Definition: toolDef("patch_file", "Apply unified diff(s); prefer replace_in_file for most edits. Use exact context lines from a fresh read_file, covering the entire edited region (never truncate before closing braces); keep hunks small and self-contained. Never include '***' markers, diff -c range headers, or copied patch-transcript text.",
			toolSchema(map[string]any{
				"diff":    toolProp("string", "Unified diff: '--- a/x'/'+++ b/x' headers, '@@ -start,count +start,count @@' hunks (context=space, removed=-, added=+). Counts must match the hunk body. Multi-file: stack sections."),
				"dry_run": toolProp("boolean", "Preview without writing"),
				"fuzzy":   toolProp("boolean", "Tolerate whitespace/shift drift (default true; leave on)"),
			}, "diff")),
		Handler:   handlePatchFileWithRetryPolicy,
		MutatesFS: true,
	},
	{
		Definition: toolDef("show_diff", "Show git diff (working tree or path).",
			toolSchema(map[string]any{
				"path":   toolProp("string", "Optional file/dir path"),
				"staged": toolProp("boolean", "Show staged changes only"),
			})),
		Handler:     handleShowDiff,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("search_code", "Search codebase (regex or literal, case-sensitive by default; set ignore_case to match any casing); returns file:line:content.",
			toolSchema(map[string]any{
				"pattern":       toolProp("string", "Regex or literal"),
				"path":          toolProp("string", "Subdirectory (required for hidden dirs)"),
				"glob":          toolProp("string", "Glob filter (e.g. *.go)"),
				"context_lines": toolProp("integer", "Context lines before/after each match"),
				"ignore_case":   toolProp("boolean", "Case-insensitive matching (default false)"),
			}, "pattern")),
		Handler:     handleSearchCode,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("find_symbol", "Locate symbols: kind=def (definition) or refs (references).",
			toolSchema(map[string]any{
				"kind":   toolPropEnum("string", []string{"def", "refs"}, ""),
				"symbol": toolProp("string", "Symbol name"),
				"path":   toolProp("string", "Subdirectory"),
				"glob":   toolProp("string", "Glob filter"),
			}, "kind", "symbol")),
		Handler:     handleFindSymbol,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("git", "Git history/status/diff (read-only): action=log (path, limit), status (path), or show (ref).",
			toolSchema(map[string]any{
				"action": toolPropEnum("string", []string{"log", "status", "show"}, ""),
				"path":   toolProp("string", "Scope to path (log/status)"),
				"limit":  toolProp("integer", "Max commits for log (default 20, max 100)"),
				"ref":    toolProp("string", "Ref/range for show (empty=HEAD)"),
			}, "action")),
		Handler:     handleGit,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("web_search", "Web search.",
			toolSchema(map[string]any{
				"query":       toolProp("string", "Query"),
				"max_results": toolProp("integer", "Max results (default 10, max 20)"),
			}, "query")),
		Handler:     handleWebSearch,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("web_fetch", "Fetch a web page: article as Markdown, full page if no article, raw for source/data files.",
			toolSchema(map[string]any{
				"url":       toolProp("string", "URL (https)"),
				"max_bytes": toolProp("integer", "Max bytes (default 262144)"),
				"selector":  toolProp("string", "Optional CSS selector (article, pre, table…)"),
				"query":     toolProp("string", "Optional case-insensitive text search"),
				"context":   toolProp("integer", "Context lines per query match (default 3, max 10)"),
			}, "url")),
		Handler:     handleWebFetch,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("download_file", "Download a file from a URL into the workspace (raw bytes, binary-safe). HTTPS-only; private/internal hosts blocked.",
			toolSchema(map[string]any{
				"url":       toolProp("string", "URL (https)"),
				"path":      toolProp("string", "Destination path"),
				"max_bytes": toolProp("integer", "Max bytes (default 52428800, max 209715200)"),
				"overwrite": toolProp("boolean", "Overwrite an existing file (default false)"),
			}, "url", "path")),
		Handler:   handleDownloadFile,
		MutatesFS: true,
	},
	{
		Definition: toolDef("git_commit", "Commit (requires staged files).",
			toolSchema(map[string]any{
				"message": toolProp("string", "Message"),
			}, "message")),
		Handler: handleGitCommit,
	},
	{
		Definition: toolDef("git_stage", "Stage files (empty = stage all).",
			toolSchema(map[string]any{
				"paths": toolPropArray("string", "File paths (empty = all)"),
			})),
		Handler: handleGitStage,
	},
	{
		Definition: toolDef("todo", "Manage the todo list: add (text), list, done (id), remove (id), clear.",
			toolSchema(map[string]any{
				"action": toolPropEnum("string", []string{"add", "list", "done", "remove", "clear"}, ""),
				"text":   toolProp("string", "Text for action=add"),
				"id":     toolProp("integer", "ID for action=done or action=remove"),
			}, "action")),
		Handler:     handleTodo,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("find_file", "Find files by name (case-insensitive substring). Faster than glob for known filenames.",
			toolSchema(map[string]any{
				"name":  toolProp("string", "Filename substring"),
				"path":  toolProp("string", "Subdirectory"),
				"limit": toolProp("integer", "Max results (default 50)"),
			}, "name")),
		Handler:     handleFindFile,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("session_rename", "Rename current session.",
			toolSchema(map[string]any{
				"label": toolProp("string", "Label"),
			}, "label")),
		Handler:     handleSessionRename,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("context_pin_last", "Pin last user message to survive compaction.",
			toolSchema(map[string]any{})),
		Handler:     handleContextPinLast,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("rename_symbol", "Rename symbol across files.",
			toolSchema(map[string]any{
				"old_name": toolProp("string", "Current name"),
				"new_name": toolProp("string", "New name"),
				"path":     toolProp("string", "Subdirectory"),
				"glob":     toolProp("string", "Glob filter"),
				"dry_run":  toolProp("boolean", "Preview only"),
			}, "old_name", "new_name")),
		Handler:   handleRenameSymbol,
		MutatesFS: true,
	},
	{
		Definition: toolDef("call_graph", "Call relationships for a symbol.",
			toolSchema(map[string]any{
				"symbol":    toolProp("string", "Symbol"),
				"path":      toolProp("string", "Subdirectory"),
				"glob":      toolProp("string", "Glob filter"),
				"direction": toolProp("string", "callers, callees, both, or impact (dependents + risk)"),
			}, "symbol")),
		Handler:     handleCallGraph,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("background_job", "Manage background jobs (execute_command background=true): action=status (output tail), wait (block until the job exits; on timeout the job keeps running), cancel, input (write to the job's stdin; returns output since the last input), or list (every live job in this session).",
			toolSchema(map[string]any{
				"action":         toolPropEnum("string", []string{"status", "wait", "cancel", "input", "list"}, ""),
				"job_id":         toolProp("string", "Job id returned by execute_command background=true (required for status/wait/cancel/input; omit for list)"),
				"input":          toolProp("string", "Text to write to the job's stdin (action=input)"),
				"append_newline": toolProp("boolean", "Append a newline to the input (default true)"),
				"timeout_ms":     toolProp("integer", "Max block for action=wait (default 60000, max 600000)"),
			}, "action")),
		Handler: handleBackgroundJob,
	},
	{
		Definition: toolDef("read_image", "Attach an image to this session's context (the model must support vision). The image stays in THIS session — a subagent calling read_image sees it, but the parent only receives this text report. png/jpeg/gif/webp up to 3.5 MB; SVG is XML text, use read_file. detail=low reduces vision cost.",
			toolSchema(map[string]any{
				"path":   toolProp("string", "Image file path"),
				"detail": toolPropEnum("string", []string{"auto", "low", "high"}, "Vision detail level (default auto)"),
			}, "path")),
		Handler:     handleReadImage,
		ReadOnly:    true,
		PlanAllowed: true,
	},
	{
		Definition: toolDef("git_blame", "Git blame for a file (read-only): shows which commit last modified each line (hash, author, date, content). Optional ref blames the file as of that commit instead of the working tree; pass line_start and line_end together to blame a range of a large file.",
			toolSchema(map[string]any{
				"file":       toolProp("string", "File path (workspace-relative)"),
				"ref":        toolProp("string", "Blame as of this commit/ref instead of the working tree"),
				"line_start": toolProp("integer", "First line of the blame range (requires line_end)"),
				"line_end":   toolProp("integer", "Last line of the blame range (requires line_start)"),
			}, "file")),
		Handler:     handleGitBlame,
		ReadOnly:    true,
		PlanAllowed: true,
	},
}

// BuiltinTools returns built-in tool definitions for the LLM, in registration
// order.
func BuiltinTools() []llm.Tool {
	tools := make([]llm.Tool, 0, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		tools = append(tools, d.Definition)
	}
	return tools
}

// parallelSafeTools is derived from the ReadOnly flag on builtinToolDefs, so
// a tool is parallel-safe by declaration in its ToolDef instead of by
// remembering to edit a second map. MCP tools are never parallel-safe (their
// side effects are unknown), and tool names shadowed by an MCP server are
// excluded at call time (toolCallsParallelEligible).
var parallelSafeTools = deriveParallelSafeTools()

func deriveParallelSafeTools() map[string]bool {
	out := make(map[string]bool, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		if d.ReadOnly {
			out[d.Definition.Name] = true
		}
	}
	return out
}

// FSMutatingToolNames returns the names of builtin tools flagged MutatesFS —
// the handlers the server wraps with the workspace fsMu. Exported so the
// server derives its lock set from the registry instead of a second map.
func FSMutatingToolNames() []string {
	names := make([]string, 0, 8)
	for _, d := range builtinToolDefs {
		if d.MutatesFS {
			names = append(names, d.Definition.Name)
		}
	}
	return names
}

// maxParallelTools bounds how many tool calls run concurrently within one
// turn (bounded by the tool batch size).
const maxParallelTools = 4
