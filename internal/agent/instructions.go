package agent

import (
	"log"

	"gogen/internal/projectfile"
)

// SetInstructionsEnabled toggles AGENTS.md/CLAUDE.md loading for this
// agent (set at construction; the web settings sweep toggles it live, and
// follows an enable with RefreshWorkspaceInstructions so the section is not
// left empty).
func (a *Agent) SetInstructionsEnabled(on bool) {
	a.instructionsEnabled.Store(on)
}

// InstructionsEnabled reports whether AGENTS.md/CLAUDE.md loading is on.
func (a *Agent) InstructionsEnabled() bool {
	return a.instructionsEnabled.Load()
}

// RefreshWorkspaceInstructions re-renders the AGENTS.md/CLAUDE.md section
// from dir into workspaceInstructions. Called at construction and after
// every working-dir change; no-op when the feature is off. Discovery
// skips missing roots and unreadable files (never an error).
func (a *Agent) RefreshWorkspaceInstructions(dir string) {
	if !a.instructionsEnabled.Load() {
		return
	}
	instr, err := projectfile.LoadInstructions(dir)
	if err != nil {
		log.Printf("warning: agent_instructions: %v", err)
		return
	}
	a.instructionsMu.Lock()
	a.workspaceInstructions = instr
	a.instructionsMu.Unlock()
}

// EffectiveGuidelines returns the project guidelines with the workspace
// instruction section appended below, rendered from the CURRENT working
// dir. Thread-safe; used by the view builders (buildSystemView /
// buildSystemSuffix).
func (a *Agent) EffectiveGuidelines() string {
	if !a.instructionsEnabled.Load() {
		return a.ProjectGuidelines
	}
	a.instructionsMu.RLock()
	instr := a.workspaceInstructions
	a.instructionsMu.RUnlock()
	if instr == "" {
		return a.ProjectGuidelines
	}
	if a.ProjectGuidelines == "" {
		return instr
	}
	return a.ProjectGuidelines + "\n\n" + instr
}
