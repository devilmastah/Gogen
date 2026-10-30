package agent

import (
	"strings"
)

// SlashCommand describes a user-facing slash command.
type SlashCommand struct {
	Name        string // e.g. "/resume"
	Description string
	Web         bool
	TUI         bool
}

// SlashCommands is the shared registry for help and autocomplete.
var SlashCommands = []SlashCommand{
	{Name: "/help", Description: "Show available commands", Web: true, TUI: true},
	{Name: "/plan", Description: "Switch to plan (read-only) mode", Web: true, TUI: true},
	{Name: "/act", Description: "Switch to act mode", Web: true, TUI: true},
	{Name: "/mode", Description: "Show current mode", Web: true, TUI: true},
	{Name: "/models", Description: "List or switch models", Web: true, TUI: true},
	{Name: "/context", Description: "Context usage details", Web: true, TUI: true},
	{Name: "/new", Description: "Start a new session", Web: true, TUI: true},
	{Name: "/resume", Description: "List, restore, or delete sessions", Web: true, TUI: true},
	{Name: "/open", Description: "Open a new live session (TUI)", Web: false, TUI: true},
	{Name: "/switch", Description: "Switch between live sessions (TUI)", Web: false, TUI: true},
	{Name: "/fork", Description: "Fork a new session from the last assistant message (or fork <N> from message N)", Web: true, TUI: true},
	{Name: "/compact", Description: "Compact conversation history", Web: false, TUI: true},
	{Name: "/smartcompact", Description: "Smart compact on, off, or toggle", Web: false, TUI: true},
	{Name: "/contextmode", Description: "Context mode: default or deep_dive", Web: false, TUI: true},
	{Name: "/verbose", Description: "Toggle verbose tool output", Web: false, TUI: true},
	{Name: "/think", Description: "Set thinking/reasoning level (off/low/medium/high) or show current level", Web: true, TUI: true},
	{Name: "/save-config", Description: "Write config to .gogen/", Web: false, TUI: true},
	{Name: "/exit", Description: "Quit GoGen", Web: false, TUI: true},
}

// HandleHelpCommand processes /help and help.
// Pass web/tui to filter which commands appear in the listing.
func HandleHelpCommand(input string, web, tui bool) (string, bool) {
	trimmed := strings.TrimSpace(input)
	switch trimmed {
	case "/help", "help":
		return FormatSlashHelp(web, tui), true
	default:
		return "", false
	}
}

// FormatSlashHelp returns a formatted help listing.
// When webOnly is true, only web-available commands are included.
// When tuiOnly is true, only TUI-available commands are included.
// Both true includes the union (commands available in either UI).
func FormatSlashHelp(web, tui bool) string {
	var b strings.Builder
	b.WriteString("Slash commands:\n")
	for _, cmd := range SlashCommands {
		if (web && cmd.Web) || (tui && cmd.TUI) {
			b.WriteString("  ")
			b.WriteString(cmd.Name)
			b.WriteString("  —  ")
			b.WriteString(cmd.Description)
			b.WriteByte('\n')
		}
	}
	if tui {
		b.WriteString("  dir <path>  —  Change working directory\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// MatchSlashCommands returns slash commands whose name starts with prefix.
// prefix should include the leading slash (e.g. "/res").
func MatchSlashCommands(prefix string, web, tui bool) []SlashCommand {
	prefix = strings.ToLower(strings.TrimSpace(prefix))
	if prefix == "" {
		return nil
	}
	var out []SlashCommand
	for _, cmd := range SlashCommands {
		if !((web && cmd.Web) || (tui && cmd.TUI)) {
			continue
		}
		if strings.HasPrefix(strings.ToLower(cmd.Name), prefix) {
			out = append(out, cmd)
		}
	}
	return out
}

// SlashCommandCompletions returns matching command names for tab completion.
func SlashCommandCompletions(line string, web, tui bool) []string {
	trimmed := strings.TrimRight(line, " \t")
	if !strings.HasPrefix(trimmed, "/") {
		return nil
	}
	// Only complete the command token (no args yet).
	if strings.ContainsAny(trimmed[1:], " \t") {
		return nil
	}
	matches := MatchSlashCommands(trimmed, web, tui)
	if len(matches) == 0 {
		return nil
	}
	out := make([]string, len(matches))
	for i, m := range matches {
		out[i] = m.Name
	}
	return out
}
