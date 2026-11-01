package agent

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"gogen/internal/llm"
	"gogen/internal/spill"
)

// MCPToolRegistry exposes MCP tools to the agent.
type MCPToolRegistry interface {
	Definitions() []llm.Tool
	ToolNames() map[string]struct{}
	// HasTool reports whether the registry exposes name. It exists so the
	// per-call membership checks do not copy the whole name map: feature-tool
	// assembly and the parallel-eligibility scan each tested names in a loop,
	// and every test allocated a fresh map of every registered tool.
	HasTool(name string) bool
	CallTool(ctx context.Context, name string, args map[string]any) (string, error)
}

// ToolHandler executes a builtin tool given parsed arguments.
type ToolHandler func(ctx context.Context, a *Agent, args map[string]any) (string, error)

// BuiltinToolHandlers returns the registry of builtin tool implementations.
// It is derived from builtinToolDefs, so every tool's schema and handler are
// declared in exactly one place and can never drift apart.
func BuiltinToolHandlers() map[string]ToolHandler {
	handlers := make(map[string]ToolHandler, len(builtinToolDefs))
	for _, d := range builtinToolDefs {
		handlers[d.Definition.Name] = d.Handler
	}
	return handlers
}

// SetToolHandlers replaces the builtin tool registry. The web server uses
// this to wrap FS-mutating tools with a workspace-level filesystem lock so
// concurrent sessions serialize actual file mutations without serializing
// whole turns. Calling with nil restores the
// builtin handlers on the next executeTool.
func (a *Agent) SetToolHandlers(handlers map[string]ToolHandler) {
	a.toolMu.Lock()
	a.toolHandlers = handlers
	a.toolMu.Unlock()
}

// mcpRegistryHas reports whether the attached MCP registry exposes a tool
// with the given name (nil-safe). Distinct from isMCPTool, which detects
// the "mcp_" name prefix; this checks the actual registered tool set, so a
// registry tool named "board" or "subagent" is visible here.
func (a *Agent) mcpRegistryHas(name string) bool {
	if a.MCPRegistry == nil {
		return false
	}
	return a.MCPRegistry.HasTool(name)
}

// featureTool is one feature-gated tool: its model-facing definition, its
// execution handler, and whether it may run in plan mode.
type featureTool struct {
	Name        string
	Definition  llm.Tool
	Handler     ToolHandler
	PlanAllowed bool
}

// featureTools is the single gating policy for the feature tools (board,
// the subagent cascade, skill): it is the one place that decides which
// feature tools are currently available on the agent, and carries each
// tool's definition, handler, and plan-mode flag. All three tool surfaces
// derive from it — llmTools (what the model sees), executeTool (what
// actually runs), and AllowedToolNames/allowsTool (the plan-mode
// allowlist) — so they cannot drift apart: adding a feature tool means
// adding one entry here.
//
// The result is ordered (board, subagent cascade, skill) so the
// model-facing definition list is deterministic. MCP shadowing is applied
// by the callers: a registry tool with the same name wins everywhere
// (explicit user configuration), in both the definition list and
// execution.
func (a *Agent) featureTools() []featureTool {
	var out []featureTool
	if a.BoardEnabled() && a.BoardManager() != nil {
		// D7: the board tool is plan-mode unrestricted (the coordination
		// exception, like todo) — an agent may update the board in plan
		// mode so it can mark items for review.
		out = append(out, featureTool{Name: "board", Definition: boardToolDef(), Handler: handleBoard, PlanAllowed: true})
	}
	if a.SubagentsEnabled() && a.SubagentSpawner() != nil {
		cs := a.continuableSpawner()
		out = append(out, featureTool{Name: "subagent", Definition: subagentToolDef(cs != nil, a.SubagentMaxConcurrent()), Handler: handleSubagent})
		if cs != nil {
			out = append(out,
				featureTool{Name: "subagent_fork", Definition: subagentForkToolDef(), Handler: handleSubagentFork},
				featureTool{Name: "list_agents", Definition: listAgentsToolDef(), Handler: handleListAgents},
				featureTool{Name: "get_agent_result", Definition: getAgentResultToolDef(), Handler: handleGetAgentResult},
				featureTool{Name: "wait_agent", Definition: waitAgentToolDef(), Handler: handleWaitAgent},
				featureTool{Name: "send_message", Definition: sendMessageToolDef(), Handler: handleSendMessage},
				featureTool{Name: "interrupt_agent", Definition: interruptAgentToolDef(), Handler: handleInterruptAgent},
			)
		}
	}
	if a.SkillsEnabled() && a.SkillsManager() != nil {
		// D-skill: skill list/read are read-only, so the skill tool is
		// plan-mode allowed like the board (the model can consult skills
		// while planning).
		out = append(out, featureTool{Name: "skill", Definition: skillsToolDef(), Handler: handleSkill, PlanAllowed: true})
	}
	if a.TerminalEnabled() {
		// Persistent terminal: one owner-scoped PTY tool with an action enum
		// (open/send/read/signal/list/close) plus the opt-in persistent
		// shell. Both are act-mode only (they run commands / mutate session
		// state), so neither sets PlanAllowed. Ordered so the model-facing
		// definition list is deterministic.
		out = append(out,
			featureTool{Name: "terminal", Definition: terminalToolDef(), Handler: handleTerminal},
			featureTool{Name: "bash_persistent", Definition: bashPersistentToolDef(), Handler: handleBashPersistent},
		)
	}
	if a.CodeIndexEnabled() && codeIndexProviderSet(a.codeIndexCfg()) {
		// Plan mode may search. The only writes are under .gogen/index.
		out = append(out, featureTool{
			Name:        "search_meaning",
			Definition:  searchMeaningToolDef(),
			Handler:     handleSearchMeaning,
			PlanAllowed: true,
		})
	}
	if a.mcpGatewayEnabled() {
		// Nested MCP (mcp_tool_mode: nested): three fixed tools. Act-mode
		// only, matching the plan-mode gate on inlined MCP tools (isMCPTool).
		// The definitions are constant, so the request prefix stays
		// byte-stable across turns and sessions.
		out = append(out, mcpGatewayFeatureTools()...)
	}
	return out
}

// featureToolFor returns the feature tool registered under name, reporting
// false when the name is not a currently available feature tool or is
// shadowed by an MCP registry tool (a registry tool with the same name
// wins: explicit user configuration).
func (a *Agent) featureToolFor(name string) (featureTool, bool) {
	if a.mcpRegistryHas(name) {
		return featureTool{}, false
	}
	for _, ft := range a.featureTools() {
		if ft.Name == name {
			return ft, true
		}
	}
	return featureTool{}, false
}

func (a *Agent) executeTool(ctx context.Context, tc llm.ToolCall) (string, error) {
	if tc.ArgsError != "" {
		return "", fmt.Errorf("invalid tool arguments: %s", tc.ArgsError)
	}
	// Self-healing (healToolCall): normalize known model naming slips
	// (misspelled tool/parameter names) before any plan-mode gating or
	// routing, so a healed call is checked and dispatched exactly as if
	// the canonical form had been emitted.
	healedName, healedArgs, healNote := healToolCall(tc.Name, tc.Args)
	tc.Name, tc.Args = healedName, healedArgs
	run := func() (string, error) {
		if err := a.checkPlanMode(tc.Name); err != nil {
			return "", err
		}
		if err := ctx.Err(); err != nil {
			return "", err
		}
		ctx = a.toolContext(ctx)
		// Feature-gated tools are routed explicitly (MCP-style) through the
		// single gating policy (featureTools): the same conditions that decide
		// the model-facing definitions (llmTools) decide what runs here, so
		// what the model sees and what executes always agree. When a feature
		// is off, the name falls through to the handler map and returns
		// "unknown tool" naturally — a stale mid-turn call after a live toggle
		// sees exactly that. A registry tool with the same name wins (explicit
		// user configuration), as it does in the definition list.
		if ft, ok := a.featureToolFor(tc.Name); ok {
			return ft.Handler(ctx, a, tc.Args)
		}
		if a.mcpRegistryHas(tc.Name) {
			// A raw server tool name stays callable even in nested mode, where
			// those definitions are not advertised: the registry really has it,
			// and the gateway resolves to this same dispatch. Plan mode still
			// refuses it above (isMCPTool), so this is not a gate bypass.
			return a.MCPRegistry.CallTool(ctx, tc.Name, tc.Args)
		}
		a.toolMu.RLock()
		handlers := a.toolHandlers
		a.toolMu.RUnlock()
		if handlers == nil {
			handlers = BuiltinToolHandlers()
		}
		h, ok := handlers[tc.Name]
		if !ok {
			return "", fmt.Errorf("unknown tool: %s", tc.Name)
		}
		return h(ctx, a, tc.Args)
	}
	result, err := run()
	if err == nil && healNote != "" {
		// Surface the healing as an event on the tool result so the model
		// sees the canonical name and stops slipping.
		result = healNote + result
	}
	return result, err
}

func handleListFiles(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	recursive, _ := boolArg(args, "recursive", false)
	tracked, _ := boolArg(args, "tracked_only", false)
	return a.Executor.ListFiles(ctx, path, recursive, tracked)
}

func handleGlobFiles(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	pattern, err := stringArg(args, "pattern")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	tracked, _ := boolArg(args, "tracked_only", false)
	return a.Executor.GlobFiles(ctx, pattern, subpath, tracked)
}

func handleRepoOverview(ctx context.Context, a *Agent, _ map[string]any) (string, error) {
	return a.Executor.RepoOverview(ctx)
}

func handleReadFile(_ context.Context, a *Agent, args map[string]any) (string, error) {
	// Prefer "path", fall back to deprecated "file_path".
	path, err := stringArgOptional(args, "path")
	if err != nil {
		return "", err
	}
	if path == "" {
		path, err = stringArgOptional(args, "file_path")
		if err != nil {
			return "", err
		}
	}
	if path == "" {
		return "", fmt.Errorf("missing required argument %q", "path")
	}
	offset, err := intArgOptional(args, "offset")
	if err != nil {
		return "", err
	}
	limit, err := intArgOptional(args, "limit")
	if err != nil {
		return "", err
	}
	search, _ := stringArgOptional(args, "search")
	lineNumbers, _ := boolArg(args, "line_numbers", false)
	return a.Executor.ReadFileRange(path, offset, limit, search, lineNumbers)
}

func handleReadFiles(_ context.Context, a *Agent, args map[string]any) (string, error) {
	paths, err := stringSliceArg(args, "paths")
	if err != nil {
		return "", err
	}
	return a.Executor.ReadFiles(paths)
}

func handleWriteFile(_ context.Context, a *Agent, args map[string]any) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	content, err := stringArg(args, "content")
	if err != nil {
		return "", err
	}
	if err := a.Executor.WriteFile(path, content); err != nil {
		return "", err
	}
	result := a.Executor.AppendSyntaxCheck("File written successfully", path)
	// No diff shown: write_file only creates new files. Use show_diff
	// after staging/committing or patch_file for modifications.
	return result, nil
}

func handleExecuteCommand(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	command, err := stringArg(args, "command")
	if err != nil {
		return "", err
	}
	background, err := boolArg(args, "background", false)
	if err != nil {
		return "", err
	}
	if background {
		// ctx carries the tool call's live-output sink/end callbacks:
		// the job streams to the UI's terminal for its whole lifetime
		// (it outlives this turn), not just while the call is in flight.
		id, err := a.StartBackgroundCommand(ctx, command)
		if err != nil {
			return "", err
		}
		return fmt.Sprintf("Started background job %s.\nCommand: %s\nWait for it with background_job (action=wait, job_id: %q), poll with action=status, list all with action=list, or cancel with action=cancel.", id, command, id), nil
	}
	// Spill target: the executor tees any output past the in-memory cap to
	// the session's spill dir and reports a head/tail preview + locator
	// instead of silently dropping the tail (best-effort; nil when this
	// session has no spill store). Background jobs are not spilled: their
	// bounded-tail buffers are the documented contract there.
	if t := a.commandSpillTarget("execute_command"); t != nil {
		ctx = spill.ContextWithTarget(ctx, t)
	}
	return a.Executor.ExecuteCommand(ctx, command)
}

func handleBackgroundJob(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	action, err := stringArg(args, "action")
	if err != nil {
		return "", err
	}
	// list is the only action that does not address one job, so job_id is
	// resolved lazily (the schema only requires action) and a missing id
	// fails with the action that needed it.
	if action == "list" {
		return a.BackgroundJobsList(), nil
	}
	jobID, err := stringArg(args, "job_id")
	if err != nil {
		return "", fmt.Errorf("background_job action=%s: %w", action, err)
	}
	switch action {
	case "status":
		return a.BackgroundJobStatus(jobID)
	case "wait":
		timeout, err := backgroundWaitTimeout(args)
		if err != nil {
			return "", err
		}
		return a.BackgroundJobWait(ctx, jobID, timeout)
	case "cancel":
		return a.CancelBackgroundJob(jobID)
	case "input":
		input, err := stringArg(args, "input")
		if err != nil {
			return "", err
		}
		appendNewline, err := boolArg(args, "append_newline", true)
		if err != nil {
			return "", err
		}
		return a.BackgroundJobInput(ctx, jobID, input, appendNewline)
	default:
		return "", fmt.Errorf("unknown background_job action %q (want status, wait, cancel, input, or list)", action)
	}
}

// backgroundWaitTimeout resolves the optional timeout_ms argument for
// action=wait (terminalSendTimeout's sibling): absent or non-positive picks
// the default, and an oversized value is clamped to the cap.
func backgroundWaitTimeout(args map[string]any) (time.Duration, error) {
	ms, err := intArgOptional(args, "timeout_ms")
	if err != nil {
		return 0, err
	}
	if ms <= 0 {
		return backgroundWaitDefaultTimeout, nil
	}
	d := time.Duration(ms) * time.Millisecond
	if d > backgroundWaitMaxTimeout {
		d = backgroundWaitMaxTimeout
	}
	return d, nil
}

func handleReplaceInFile(_ context.Context, a *Agent, args map[string]any) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	search, err := stringArg(args, "search")
	if err != nil {
		return "", err
	}
	replace, err := stringArg(args, "replace")
	if err != nil {
		return "", err
	}
	replaceAll, err := boolArg(args, "replace_all", false)
	if err != nil {
		return "", err
	}
	return a.Executor.ReplaceInFile(path, search, replace, replaceAll)
}

func handlePatchFile(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	diff, err := stringArg(args, "diff")
	if err != nil {
		return "", err
	}
	dryRun, err := boolArg(args, "dry_run", false)
	if err != nil {
		return "", err
	}
	fuzzy, err := boolArg(args, "fuzzy", true)
	if err != nil {
		return "", err
	}
	return a.Executor.PatchFile(ctx, diff, dryRun, fuzzy)
}

// handlePatchFileWithRetryPolicy wraps handlePatchFile with the
// patch_file retry-loop policy so executeTool stays a generic dispatcher.
// It is the handler registered under "patch_file" in builtinToolDefs.
//
// Pre-execution: a diff that consists solely of patch framing — end
// delimiters ("*** End of diff" …) and failure narration, with no headers
// or hunks — is a degenerate retry-loop reply: the model has dropped every
// hunk and is re-emitting only framing text. Do not even attempt to apply
// it; errPatchTurnStop makes runToolRound end the turn so the model cannot
// loop again.
//
// Post-execution: only stale-diff failures (ErrPatchMismatch) count toward
// the streak — permission, I/O, and path-safety errors mean the diff may be
// fine and the environment is the problem, so the "regenerate the diff"
// hint would be misleading. Success and non-mismatch errors both reset.
func handlePatchFileWithRetryPolicy(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	if diff, ok := args["diff"].(string); ok && detectMarkerOnlyDiff(diff) {
		return "", fmt.Errorf("%w: patch_file received a diff containing only patch markers and no patch content — the model appears stuck in a patch retry loop; stopping the turn. Re-read the target file(s) with read_file and regenerate the diff",
			errPatchTurnStop)
	}
	res, err := handlePatchFile(ctx, a, args)
	if err != nil && errors.Is(err, ErrPatchMismatch) {
		// Per-turn hard stop, keyed on the failure: strikes accumulate
		// only while the SAME diff keeps failing (same target, same
		// mismatch). A model iterating across different files or diffs
		// is making progress and must not be stopped — only a model
		// re-sending the same broken diff is looping. errPatchTurnStop
		// makes runToolRound end the turn instead of letting the model
		// write another attempt.
		key := res
		if key == "" {
			key = err.Error()
		}
		if prev, _ := a.patchStrikeKey.Load().(string); prev != key {
			a.patchStrikeKey.Store(key)
			a.patchTurnStrikes.Store(1)
		} else if strikes := a.patchTurnStrikes.Add(1); strikes >= 3 {
			return res, fmt.Errorf("%w: patch_file failed %d times in a row with the same diff; stopping the turn. Re-read the target file(s) with read_file (or search_code) and regenerate the diff from their current content",
				errPatchTurnStop, strikes)
		}
		streak := a.patchFailStreak.Add(1)
		if streak > 3 {
			streak = 3 // keep the message from escalating forever
		}
		if streak >= 2 {
			err = fmt.Errorf("patch_file failed %d times in a row: %w. Do not retry the same diff — it will keep failing. Re-read the target file(s) with read_file (or search_code) and regenerate the diff from their current content. If a small file keeps failing, use replace_in_file or write_file instead",
				streak, err)
		}
	} else {
		a.patchFailStreak.Store(0)
		a.patchTurnStrikes.Store(0)
		a.patchStrikeKey.Store("")
	}
	return res, err
}

func handleDelete(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	return a.Executor.DeleteFile(ctx, path)
}

func handleShowDiff(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	subpath, _ := stringArgOptional(args, "path")
	staged, err := boolArg(args, "staged", false)
	if err != nil {
		return "", err
	}
	return a.Executor.ShowDiff(ctx, subpath, staged)
}

func handleSearchCode(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	pattern, err := stringArg(args, "pattern")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	glob, _ := stringArgOptional(args, "glob")
	contextLines, err := intArgOptional(args, "context_lines")
	if err != nil {
		return "", err
	}
	ignoreCase, err := boolArg(args, "ignore_case", false)
	if err != nil {
		return "", err
	}
	return a.Executor.SearchCode(ctx, pattern, subpath, glob, contextLines, ignoreCase)
}

func handleFindSymbol(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	kind, err := stringArg(args, "kind")
	if err != nil {
		return "", err
	}
	symbol, err := stringArg(args, "symbol")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	glob, _ := stringArgOptional(args, "glob")
	switch kind {
	case "def":
		return a.Executor.FindDefinition(ctx, symbol, subpath, glob)
	case "refs":
		return a.Executor.FindReferences(ctx, symbol, subpath, glob)
	default:
		return "", fmt.Errorf("unknown find_symbol kind %q (want def or refs)", kind)
	}
}

func handleGitStage(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	paths, err := stringSliceArgOptional(args, "paths")
	if err != nil {
		return "", err
	}
	return a.Executor.GitStage(ctx, paths)
}

func handleGitCommit(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	message, err := stringArg(args, "message")
	if err != nil {
		return "", err
	}
	return a.Executor.GitCommit(ctx, message)
}

func handleGit(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	action, err := stringArg(args, "action")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	switch action {
	case "log":
		limit, err := intArgOptional(args, "limit")
		if err != nil {
			return "", err
		}
		return a.Executor.GitLog(ctx, subpath, limit)
	case "status":
		return a.Executor.GitStatus(ctx, subpath)
	case "show":
		ref, _ := stringArgOptional(args, "ref")
		return a.Executor.GitDiffShow(ctx, ref)
	default:
		return "", fmt.Errorf("unknown git action %q (want log, status, or show)", action)
	}
}

func handleGitBlame(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	file, err := stringArg(args, "file")
	if err != nil {
		return "", err
	}
	ref, _ := stringArgOptional(args, "ref")
	lineStart, err := intArgOptional(args, "line_start")
	if err != nil {
		return "", err
	}
	lineEnd, err := intArgOptional(args, "line_end")
	if err != nil {
		return "", err
	}
	return a.Executor.GitBlame(ctx, file, ref, lineStart, lineEnd)
}

func handleTodo(_ context.Context, a *Agent, args map[string]any) (string, error) {
	action, err := stringArg(args, "action")
	if err != nil {
		return "", err
	}
	tm, err := a.todo()
	if err != nil {
		return "", err
	}
	switch action {
	case "add":
		text, err := stringArg(args, "text")
		if err != nil {
			return "", err
		}
		out, err := tm.AddTodo(text)
		if err != nil {
			return "", err
		}
		a.persistTodos()
		return out, nil
	case "list":
		return tm.ListTodos(), nil
	case "done":
		id, err := intRequiredArg(args, "id")
		if err != nil {
			return "", err
		}
		out, err := tm.DoneTodo(id)
		if err != nil {
			return "", err
		}
		a.persistTodos()
		return out, nil
	case "remove":
		id, err := intRequiredArg(args, "id")
		if err != nil {
			return "", err
		}
		out, err := tm.RemoveTodo(id)
		if err != nil {
			return "", err
		}
		a.persistTodos()
		return out, nil
	case "clear":
		out, err := tm.ClearDoneTodos()
		if err != nil {
			return "", err
		}
		a.persistTodos()
		return out, nil
	default:
		return "", fmt.Errorf("unknown todo action %q (want add, list, done, remove, or clear)", action)
	}
}

func handleListDefinitions(_ context.Context, a *Agent, args map[string]any) (string, error) {
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	return a.Executor.ListDefinitions(path)
}

func handleWebSearch(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	query, err := stringArg(args, "query")
	if err != nil {
		return "", err
	}
	maxResults, err := intArgOptional(args, "max_results")
	if err != nil {
		return "", err
	}
	return a.Executor.WebSearch(ctx, query, maxResults)
}

func handleWebFetch(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	rawURL, err := stringArg(args, "url")
	if err != nil {
		return "", err
	}
	maxBytes, err := intArgOptional(args, "max_bytes")
	if err != nil {
		return "", err
	}
	selector, err := stringArgOptional(args, "selector")
	if err != nil {
		return "", err
	}
	query, err := stringArgOptional(args, "query")
	if err != nil {
		return "", err
	}
	contextLines, err := intArgOptional(args, "context")
	if err != nil {
		return "", err
	}
	return a.Executor.WebFetch(ctx, rawURL, WebFetchOptions{
		MaxBytes: maxBytes,
		Selector: selector,
		Query:    query,
		Context:  contextLines,
	})
}

func handleDownloadFile(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	rawURL, err := stringArg(args, "url")
	if err != nil {
		return "", err
	}
	path, err := stringArg(args, "path")
	if err != nil {
		return "", err
	}
	maxBytes, err := intArgOptional(args, "max_bytes")
	if err != nil {
		return "", err
	}
	overwrite, err := boolArg(args, "overwrite", false)
	if err != nil {
		return "", err
	}
	return a.Executor.DownloadFile(ctx, rawURL, path, maxBytes, overwrite)
}

func handleFindFile(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	name, err := stringArg(args, "name")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	limit, _ := intArgOptional(args, "limit")
	return a.Executor.FindFile(ctx, name, subpath, limit)
}

func handleSessionRename(_ context.Context, a *Agent, args map[string]any) (string, error) {
	label, err := stringArg(args, "label")
	if err != nil {
		return "", err
	}
	return a.RenameSession(label)
}

func handleContextPinLast(_ context.Context, a *Agent, _ map[string]any) (string, error) {
	if a.PinManager == nil {
		// PinManager is unconditionally initialised in main(); this branch
		// only fires in tests / custom embeds. Tell the LLM the tool is a
		// no-op rather than returning an error that halts the turn.
		return "Pin manager not configured; context pinning is a no-op.", nil
	}
	return "Pinned the last user message", a.pinLastUser()
}

func handleRenameSymbol(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	oldName, err := stringArg(args, "old_name")
	if err != nil {
		return "", err
	}
	newName, err := stringArg(args, "new_name")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	glob, _ := stringArgOptional(args, "glob")
	dryRun, _ := boolArg(args, "dry_run", false)
	return a.Executor.RenameSymbol(ctx, oldName, newName, subpath, glob, dryRun)
}

func handleCallGraph(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	symbol, err := stringArg(args, "symbol")
	if err != nil {
		return "", err
	}
	subpath, _ := stringArgOptional(args, "path")
	glob, _ := stringArgOptional(args, "glob")
	direction, _ := stringArgOptional(args, "direction")
	return a.Executor.CallGraph(ctx, symbol, subpath, glob, direction)
}

// toolRegistry groups the live tool-handler map. The toolMu comment carries
// the swap-publication contract.
type toolRegistry struct {
	// toolMu guards toolHandlers. SetToolHandlers is called at construction
	// (server startup / per-session agent factory) before any turn runs, but
	// executeTool reads the map on every tool call, so the swap is published
	// under the lock to keep the read/write race-free by construction.
	toolMu       sync.RWMutex
	toolHandlers map[string]ToolHandler
}
