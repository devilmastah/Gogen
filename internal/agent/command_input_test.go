package agent

import "testing"

// TestIsCommandInput pins the shared busy-gate classification. The two front
// ends used to keep separate lists and disagreed: bare "think"/"models" were
// commands on the web but chat in the TUI, and bare "verbose"/"compact"/
// "queue" the reverse. One union list means a word that is a command on
// either surface is never queued to the model as a prompt.
func TestIsCommandInput(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"plain chat", "fix the parser bug", false},
		{"slash command", "/new", true},
		{"slash with args", "/resume 3", true},
		{"empty", "", false},

		// Words every surface dispatches when bare.
		{"bare help", "help", true},
		{"bare think (was chat in the TUI)", "think", true},
		{"bare models (was chat in the TUI)", "models", true},
		{"bare compact (was chat on the web)", "compact", true},
		{"bare verbose (was chat on the web)", "verbose", true},
		{"bare queue (was chat on the web)", "queue", true},
		{"bare save-config", "save-config", true},
		{"bare switch", "switch", true},

		// Argument-taking commands decide on the first word.
		{"think with level", "think high", true},
		{"models selector", "models gpt-4o", true},
		{"resume arg", "resume latest", true},
		{"fork arg", "fork step 3", true},
		{"dir arg", "dir ..", true},
		{"switch arg", "switch 2", true},

		// Exact-only commands with trailing text are chat: the dispatchers
		// match the whole trimmed input, so this text would reach the model.
		{"help with args", "help me debug this", false},
		{"plan with args", "plan out the refactor", false},
		{"mode with args", "mode of operation is unclear", false},
		{"new with args", "new approach please", false},
		{"context with args", "context of this file", false},

		// A word that merely starts with a command word is chat. "switch" in
		// particular used to be a bare HasPrefix entry in the TUI list, so
		// "switchboard is down" was refused as a command mid-turn.
		{"switchboard", "switchboard is down", false},
		{"news", "news about the build", false},
		{"forklift", "forklift is broken", false},
		{"thinker", "thinker needed", false},
		{"modelsx", "modelsx is not a command", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsCommandInput(tc.in); got != tc.want {
				t.Fatalf("IsCommandInput(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
