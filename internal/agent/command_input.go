package agent

import "strings"

// Bare command words: a word that, alone, is a command in one of the front
// ends (the TUI command table, the web turn dispatch, or one of the shared
// parsers ParseSessionCommand / ParseModelsCommand).
//
// The list is the UNION of both front ends on purpose. Each surface used to
// keep its own copy, and they disagreed: a bare "think" or "models" is a
// command on the web but was queued to the model as chat in the TUI, and the
// reverse held for "verbose", "compact" and "queue". The only consumer is the
// busy gate (typed input while a turn runs: command-shaped input is refused
// with a hint, plain text is queued as steering), so the union is the safe
// direction — a word that is a command anywhere must not be delivered to the
// model as a prompt.
var bareCommandWords = map[string]bool{
	"help": true, "exit": true, "quit": true,
	"compact": true, "smartcompact": true, "contextmode": true, "verbose": true, "subagents": true,
	"plan": true, "act": true, "mode": true, "think": true,
	"context": true, "models": true,
	"new": true, "sessions": true, "resume": true, "fork": true,
	"save-config": true, "switch": true, "open": true, "queue": true,
}

// Argument-taking command words: the FIRST word decides, and only when it is
// followed by arguments ("think high", "resume 3", "dir .."). Deliberately
// narrower than bareCommandWords: "plan", "act", "mode", "help", "context",
// "new" and the modal words are matched EXACTLY by every dispatcher, so
// "help me debug this" and "new approach?" stay chat instead of being refused
// as malformed commands.
var argCommandWords = map[string]bool{
	"smartcompact": true, "contextmode": true, "think": true, "models": true, "dir": true,
	"switch": true, "open": true, "sessions": true, "resume": true, "fork": true,
}

// IsCommandInput reports whether trimmed input is command-shaped rather than
// chat text: a slash command, or a bare word one of the front ends dispatches
// as a command (see bareCommandWords / argCommandWords).
//
// It is the single classifier both busy gates use — the web's steerableInput
// and the TUI's mid-turn submit gate — so "is this chat I can queue, or a
// command I must refuse?" cannot drift between the two surfaces again.
// Callers pass already-trimmed input; empty input is not a command.
func IsCommandInput(trimmed string) bool {
	if trimmed == "" {
		return false
	}
	if strings.HasPrefix(trimmed, "/") {
		return true
	}
	word, args, hasArgs := strings.Cut(trimmed, " ")
	if hasArgs && args != "" {
		return argCommandWords[word]
	}
	return bareCommandWords[word]
}
