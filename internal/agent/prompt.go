package agent

import (
	"strings"
	"sync"

	"gogen/internal/llm"
)

// systemPromptTemplateOnce caches the template body once. Per-working-
// directory prompts are not cached: template substitution is cheap, and
// long-lived processes that change working directories would otherwise grow
// an unbounded map.
var systemPromptTemplateOnce sync.Once
var systemPromptTmpl string

// systemPromptCfg holds the user-configured system prompt template
// ("" = the built-in default). Package state like the web/treesitter config
// setters: applied at startup (applyRuntimeConfig) and live from the web
// settings modal.
var systemPromptCfg struct {
	mu   sync.Mutex
	tmpl string
}

// ConfigureSystemPrompt sets the user-configured system prompt template
// ("" restores the built-in default). The template may contain the
// {working_dir} placeholder; the project profile, project rules, and
// plan-mode suffixes always append after it.
func ConfigureSystemPrompt(tmpl string) {
	systemPromptCfg.mu.Lock()
	systemPromptCfg.tmpl = tmpl
	systemPromptCfg.mu.Unlock()
}

func configuredSystemPrompt() string {
	systemPromptCfg.mu.Lock()
	defer systemPromptCfg.mu.Unlock()
	return systemPromptCfg.tmpl
}

// DefaultSystemPromptTemplate returns the built-in system prompt template
// (with the {working_dir} placeholder) — the settings modal's "system
// prompt" field is pre-populated with it, and SystemPrompt resolves against
// it so the default text can never be baked into a config file.
func DefaultSystemPromptTemplate() string {
	systemPromptTemplateOnce.Do(func() {
		systemPromptTmpl = systemPromptTemplate()
	})
	return systemPromptTmpl
}

// SystemPrompt returns the effective agent system prompt: the configured
// template (empty, or equal to the built-in default → built-in) with the
// {working_dir} placeholder substituted. This is the base prompt only: the
// MCP hint (when the session was configured with MCP servers), the project profile, the project
// rules, and the plan-mode suffix are appended per agent by
// buildSystemView / systemPromptPrefix.
func SystemPrompt(workingDir string) string {
	tmpl := ResolvePromptTemplate(configuredSystemPrompt(), DefaultSystemPromptTemplate())
	return strings.ReplaceAll(tmpl, "{working_dir}", workingDir)
}

// The MCP hint is appended to the system prompt only when the session was
// constructed with MCP servers configured AND the current mode exposes them:
// MCP is opt-in, so a session without one must not advertise tools it does not
// have, and plan mode does not mention MCP at all, because it withholds every
// MCP tool — a mode that cannot see them gains nothing from being told how to
// call them. The hint is chosen from CONSTRUCTION-TIME state and the mode,
// never from the live registry, because the system prompt leads the request
// prefix — a hint that appeared when a server finished connecting would
// invalidate the provider's cache for the whole conversation. Both texts are
// constants: no counts, no server names, nothing that changes between turns.
const (
	// mcpGatewayHint is the nested-mode hint (mcp_tool_mode: nested): the three
	// gateway tools in one line, in the same shape as mcpInlineHint. The call
	// contract lives in each tool's definition, so this stays a pointer.
	mcpGatewayHint = "MCP tools: use mcp_list to discover, mcp_describe for one tool's schema, " +
		"and mcp_call to run one (input holds only that tool's own parameters)."
	// mcpInlineHint is the inline-mode hint: the server tools are inlined into
	// the definition list under their mcp_* names, and the set itself comes
	// from the live registry, so one line names the family the model will see.
	mcpInlineHint = "Additional mcp_* tools may be available."
)

// contextAdviceDefault is the context advice of the built-in prompt
// (context_mode: default).
const contextAdviceDefault = `When context is
tight, pin critical facts with context_pin_last so they survive compaction, then
summarize progress in one short message.`

// contextAdviceDeepDive replaces contextAdviceDefault for context_mode:
// deep_dive while auto-compaction is on. It does not suggest
// context_pin_last: a pin keeps everything after the pinned message
// verbatim, which in a long turn leaves compaction nothing to free.
const contextAdviceDeepDive = `History is compacted
automatically when the context window fills: the user's first message, the current
request, and the latest messages stay verbatim, and the rest is summarized. Keep facts
you will need later as todo items (the todo list survives compaction; todo with action
list shows it). Do not stop or shorten your work because of context size; continue as
normal.`

// systemPromptBase is this session's base prompt for workingDir: SystemPrompt
// with the context advice for its context_mode. The deep-dive advice says
// history is compacted automatically, so it applies only while
// auto-compaction is on; with compact_threshold 0 the default advice stays.
func (a *Agent) systemPromptBase(workingDir string) string {
	deepDive := a.Context != nil && a.Context.DeepDive() && a.Context.AutoCompactEnabled()
	return contextAdvice(SystemPrompt(workingDir), deepDive)
}

// contextAdvice puts the deep-dive advice in place of the default one when
// deepDive is set. A custom template without the default sentence is left as
// it is.
func contextAdvice(prompt string, deepDive bool) string {
	if !deepDive {
		return prompt
	}
	return strings.Replace(prompt, contextAdviceDefault, contextAdviceDeepDive, 1)
}

func systemPromptTemplate() string {
	return `You are GoGen, a coding agent working in the local repository at {working_dir}.

Guidelines:
Before editing: explore with repo_overview, search_code, list_definitions. Use read_file
offset/limit to avoid loading whole files; batch reads with read_files.
You can call multiple tools in one response — batch independent read-only calls (they run
concurrently, at most 4 at a time); batches containing any edit run strictly sequentially
in call order.
Tool output is ground truth: if a read or search returns nothing or errors, adapt — never
assume file contents or paths that tools did not return.

Edits: surgical only — replace_in_file by default, patch_file for multi-hunk,
write_file create-only. If an edit fails, re-read and retry; never reuse old patch
output or transcript text in a new diff. Run tests/lints via execute_command after
edits.

Tasks: track multi-step work with todo and mark items done as you go. ` + contextAdviceDefault + `

Safety: never exfiltrate secrets. No destructive commands (rm -rf, sudo, curl|bash).
For ambiguous tasks, state assumptions and proceed. Summarize changes when done.

Docs: verify claims against code. Only document what exists — no invented config, flags,
or features. Omit unimplemented/roadmap items.`
}

// buildSystemSuffix returns the MCP-hint / project-profile / project-rules /
// plan-mode suffix text that buildSystemView folds into the leading system
// message. Split out so systemPromptPrefix can construct the wire prefix
// without copying the whole message history.
//
// mcpHint is the session's MCP hint line (a.mcpToolHint()), or "" when the
// session has no MCP servers configured or the current mode withholds them: the
// hint is emitted only when there is something to describe, so the base prompt
// of an MCP-less or plan-mode session never advertises MCP tools. The hint
// leads the suffix because it qualifies the base prompt rather than the
// project-specific enrichment below it.
func buildSystemSuffix(projectFilePath, guidelines, projectProfile string, mode Mode, mcpHint string) string {
	var suffix strings.Builder
	// Suffixes in the same order enrichSystemPrompt applied them, with the
	// MCP hint first.
	if mcpHint != "" {
		suffix.WriteString("\n\n" + mcpHint)
	}
	if projectProfile != "" {
		suffix.WriteString("\n\nProject profile (auto-detected):\n" + projectProfile)
	}
	if guidelines != "" {
		suffix.WriteString(projectRulesHeader(projectFilePath, guidelines))
	}
	if mode == ModePlan {
		suffix.WriteString(planModePromptSuffix)
	}
	return suffix.String()
}

// buildSystemView returns messages with the system prompt prepended (or the
// existing first system message enriched), folding the session's MCP hint
// (mcpHint, empty when no MCP servers are configured), the project profile, the
// project rules, and the plan-mode suffix
// into the system message content in a SINGLE copy of the slice. The previous
// two-step pipeline
// (withSystemPrompt + enrichSystemPrompt) copied the whole message slice once
// per suffix; on long conversations that was 2-5 full shallow copies per turn
// and per ContextStats probe. The produced view is byte-identical to the old
// pipeline: the base system prompt, then the MCP hint (only when mcpHint is
// set), then the profile suffix, then the project rules header, then the plan-mode
// suffix, all on the leading system message.
func buildSystemView(messages []llm.Message, basePrompt, projectFilePath, guidelines, projectProfile string, mode Mode, mcpHint string) []llm.Message {
	if len(messages) == 0 {
		return messages
	}
	suffix := buildSystemSuffix(projectFilePath, guidelines, projectProfile, mode, mcpHint)

	// History already carries a system message (unusual — canonical history
	// is user/assistant/tool): keep the list and fold the suffixes into that
	// first system message, matching the old pipeline exactly.
	for i := range messages {
		if messages[i].Role == "system" {
			if suffix == "" {
				return messages
			}
			out := append([]llm.Message(nil), messages...)
			out[i].Content += suffix
			return out
		}
	}

	// No system message: prepend one with the full content in one copy.
	content := basePrompt + suffix
	out := make([]llm.Message, 0, len(messages)+1)
	out = append(out, llm.Message{Role: "system", Content: content})
	out = append(out, messages...)
	return out
}

func projectRulesHeader(path, guidelines string) string {
	name := path
	if name == "" {
		name = "project file"
	}
	return "\n\nProject rules (" + name + "):\n" + guidelines
}

// planModePromptSuffix is derived from the tool registry so the blocked-tool
// list can never drift from the PlanAllowed/ReadOnly flags. It is computed
// once at startup; the system-message prefix stays prompt-cache-stable per
// build, and the derivation cost is negligible.
var planModePromptSuffix = buildPlanModePromptSuffix()

func buildPlanModePromptSuffix() string {
	blocked := make([]string, 0, 8)
	for _, d := range builtinToolDefs {
		if !d.ReadOnly && !d.PlanAllowed {
			blocked = append(blocked, d.Definition.Name)
		}
	}
	return "\n\nPlan mode is active. You may explore and explain only.\n" +
		"Do not call " + strings.Join(blocked, ", ") + " tools.\n" +
		"Produce a clear, actionable plan; the user will switch to act mode to implement."
}
