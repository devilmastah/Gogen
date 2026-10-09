package agent

import (
	"strings"
	"sync"
)

// Configurable prompt templates for board-started agents and subagents.
//
// Each template is a plain string with named placeholders; an empty
// configured value — or a value EXACTLY equal to the built-in default —
// resolves to the built-in default. The default text therefore can never be
// baked into a config file ("saving the default" stores nothing) and future
// default improvements always reach users who did not customize. A value
// that merely differs (even by a word) is stored verbatim as a custom
// template; an OLD default text from a previous version no longer matches
// the current default and is treated as a pinned custom prompt.

// ResolvePromptTemplate returns the effective prompt template: the
// configured value, or the built-in default when unset OR when the value
// equals the default — a value equal to the default always means "the
// default", wherever it came from. The single implementation of this rule,
// shared by SystemPrompt, TicketPrompt, FormatSubagentJob, and the web
// settings push (server), so the semantics cannot drift between sites.
func ResolvePromptTemplate(configured, def string) string {
	if configured == "" || configured == def {
		return def
	}
	return configured
}

// NormalizePromptTemplate stores "" when a submitted prompt template equals
// the built-in default: "saving the default" must not bake the default text
// into the config file. Hand-edited files/env with the default text are
// still handled by ResolvePromptTemplate (same rule at resolution time).
func NormalizePromptTemplate(v, def string) string {
	if v == def {
		return ""
	}
	return v
}

// DefaultBoardStartPrompt is the built-in template for agents started from
// a board ticket. Placeholders: {id} {title} {description} {priority}
// {context}.
const DefaultBoardStartPrompt = `You have been assigned board ticket #{id}: {title}.

The ticket below describes work to do — it may be a bug, a feature
request, or a problem statement. Understand it, design and implement
the solution yourself, then verify your work before declaring it
complete.

{description}

Priority: {priority}

{context}

If anything is ambiguous, make a reasonable assumption and note it in
a comment on the ticket.

Use the board tool to keep the ticket updated as you work:
- comment on notable progress or blockers,
- move it to in_review when you believe it's done,
- mark it done (done {id}) once you've verified the work.

The ticket is already assigned to you and in progress — don't claim it again.`

// TicketPrompt renders the seed prompt for a board-started agent session.
// template is the user-configured template; empty (or equal to the built-in
// default) uses DefaultBoardStartPrompt. Substitution is single-pass over
// the template, so placeholder-like text inside the ticket's own fields is
// never re-substituted.
func TicketPrompt(item *BoardItem, template string) string {
	template = ResolvePromptTemplate(template, DefaultBoardStartPrompt)
	priority := item.Priority
	if priority == "" {
		priority = "none"
	}
	return strings.NewReplacer(
		"{id}", item.ID,
		"{title}", item.Title,
		"{description}", item.Description,
		"{priority}", priority,
		"{context}", ticketContextBlock(item),
	).Replace(template)
}

// ticketContextBlock renders the {context} placeholder: the ticket's explicit
// Context text (if any) followed by the activity-derived log block (if any).
// A fresh ticket with neither renders "" so the prompt stays lean.
func ticketContextBlock(item *BoardItem) string {
	var parts []string
	if c := strings.TrimSpace(item.Context); c != "" {
		parts = append(parts, c)
	}
	if b := activityContextBlock(item.Activity); b != "" {
		parts = append(parts, b)
	}
	return strings.Join(parts, "\n\n")
}

// maxActivityContextLines caps the content-bearing entries included in a
// ticket prompt.
const maxActivityContextLines = 5

// maxActivityContextLineLen caps each included entry.
const maxActivityContextLineLen = 300

// activityContext returns the content-bearing activity entries (block
// reasons and comments), skipping the generated status-transition noise
// ("created", "claimed", "moved to …", "marked done"). The transition texts
// are stable: every mutation goes through appendActivityLocked in this
// package, so the prefix filter cannot drift.
func activityContext(activity []BoardActivity) []string {
	var out []string
	for _, act := range activity {
		text := strings.TrimSpace(act.Text)
		if text == "" {
			continue
		}
		switch {
		case text == "created", text == "claimed", text == "marked done":
			continue
		case strings.HasPrefix(text, "moved to "):
			continue
		}
		if len(text) > maxActivityContextLineLen {
			text = text[:maxActivityContextLineLen] + "…"
		}
		out = append(out, text)
	}
	if len(out) > maxActivityContextLines {
		out = append([]string(nil), out[len(out)-maxActivityContextLines:]...)
	}
	return out
}

// activityContextBlock renders the activity-derived portion of the {context}
// placeholder: a labeled list of the content-bearing entries, or "" when the
// log has none (a freshly started ticket keeps the prompt lean). It is
// combined with the ticket's explicit Context by ticketContextBlock.
func activityContextBlock(activity []BoardActivity) string {
	entries := activityContext(activity)
	if len(entries) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("Ticket log context:\n")
	for _, e := range entries {
		b.WriteString("- " + e + "\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// DefaultReviewAgentPrompt is the built-in template for review agents
// auto-started on tickets moved into in_review. Placeholders: {id} {title}
// {description} {priority} {context} {assignee}.
const DefaultReviewAgentPrompt = `You are the review agent for board ticket #{id}: {title}.

The ticket was moved to in_review by {assignee}, who believes the work
is complete. Verify it: inspect the actual changes (git diff/status),
check the work against the ticket description below, and run the
project's tests/lint commands when they exist.

{description}

Priority: {priority}

{context}

Then finish the review using the board tool:
- If the work is complete and correct, mark it done (done {id}).
- If it needs changes, comment your findings as concrete, actionable
  review notes, then move the ticket back to in_progress
  (move {id} in_progress).

Rules:
- Never claim the ticket: it stays assigned to {assignee}.
- Never move the ticket to in_review yourself (that would restart the
  review loop).
- Judge what the ticket asked for. Style nits go in a comment, not in a
  rejection.
- If the ticket left in_review while you were working, comment your
  findings instead of moving or closing anything.`

// ReviewPrompt renders the seed prompt for an auto-started review session.
// template is the user-configured template; empty (or equal to the built-in
// default) uses DefaultReviewAgentPrompt. Substitution is single-pass over
// the template, so placeholder-like text inside the ticket's own fields is
// never re-substituted (same contract as TicketPrompt).
func ReviewPrompt(item *BoardItem, template string) string {
	template = ResolvePromptTemplate(template, DefaultReviewAgentPrompt)
	assignee := item.Assignee
	if assignee == "" {
		assignee = "the assignee"
	}
	priority := item.Priority
	if priority == "" {
		priority = "none"
	}
	return strings.NewReplacer(
		"{id}", item.ID,
		"{title}", item.Title,
		"{description}", item.Description,
		"{priority}", priority,
		"{assignee}", assignee,
		"{context}", ticketContextBlock(item),
	).Replace(template)
}

// DefaultSubagentPrompt is the built-in template wrapping subagent jobs.
// Placeholder: {job}. The wording makes the return contract explicit without
// promising a continuation primitive that a foreground-only host may not
// expose: the final message is the child's result. If input is required, end
// with a clear question; a continuable background child can then be resumed
// with send_message.
const DefaultSubagentPrompt = `You are a subagent working on a task delegated by the parent agent.
Work independently and completely on the job below, then finish your
turn with a concise summary of what you did and the outcome — the
parent receives that final message as the task result. If you need
input from the parent, end with a clear question. If this is a
continuable background child, the parent can reply and continue the
session; otherwise the question becomes the final report.

Job:
{job}`

// subagentPromptCfg holds the user-configured subagent job template
// ("" = the built-in default). Package state like ConfigureSystemPrompt.
var subagentPromptCfg struct {
	mu   sync.Mutex
	tmpl string
}

// ConfigureSubagentPrompt sets the user-configured subagent job template
// ("" restores the built-in default). The template may contain the {job}
// placeholder.
func ConfigureSubagentPrompt(tmpl string) {
	subagentPromptCfg.mu.Lock()
	subagentPromptCfg.tmpl = tmpl
	subagentPromptCfg.mu.Unlock()
}

func configuredSubagentPrompt() string {
	subagentPromptCfg.mu.Lock()
	defer subagentPromptCfg.mu.Unlock()
	return subagentPromptCfg.tmpl
}

// FormatSubagentJob wraps a subagent job with the configured template
// (empty, or equal to the built-in default → the built-in default) and
// substitutes the {job} placeholder. Callers (the web and TUI spawners)
// derive labels from the ORIGINAL job before wrapping, so sidebar labels
// and the subagent_started event keep the job the parent actually wrote.
func FormatSubagentJob(job string) string {
	tmpl := ResolvePromptTemplate(configuredSubagentPrompt(), DefaultSubagentPrompt)
	return strings.ReplaceAll(tmpl, "{job}", job)
}
