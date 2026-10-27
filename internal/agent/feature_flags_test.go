package agent

import (
	"testing"

	"gogen/internal/config"
	"gogen/internal/contextmgr"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/skills"
)

// TestFeatureFlagsDefaultOff verifies the live feature flags default to off
// with the default nesting depth, exactly like the config layer.
func TestFeatureFlagsDefaultOff(t *testing.T) {
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	a := NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
	if a.BoardEnabled() {
		t.Fatal("board should default off")
	}
	if a.SubagentsEnabled() {
		t.Fatal("subagents should default off")
	}
	if d := a.SubagentMaxDepth(); d != config.DefaultSubagentMaxDepth {
		t.Fatalf("default depth = %d, want %d", d, config.DefaultSubagentMaxDepth)
	}
	if n := a.SubagentMaxConcurrent(); n != config.DefaultSubagentMaxConcurrent {
		t.Fatalf("default concurrent limit = %d, want %d", n, config.DefaultSubagentMaxConcurrent)
	}
}

// TestFeatureFlagSetters verifies the setters publish atomically (run under
// -race to check concurrent readers).
func TestFeatureFlagSetters(t *testing.T) {
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	a := NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
	a.SetBoardEnabled(true)
	a.SetSubagentsEnabled(true)
	a.SetSubagentMaxDepth(3)
	if !a.BoardEnabled() || !a.SubagentsEnabled() {
		t.Fatalf("flags not set: board=%v subagents=%v", a.BoardEnabled(), a.SubagentsEnabled())
	}
	if d := a.SubagentMaxDepth(); d != 3 {
		t.Fatalf("depth = %d, want 3", d)
	}
	a.SetSubagentMaxDepth(0)
	if d := a.SubagentMaxDepth(); d != config.DefaultSubagentMaxDepth {
		t.Fatalf("zero depth should fall back to default, got %d", d)
	}
	a.SetSubagentMaxConcurrent(2)
	if n := a.SubagentMaxConcurrent(); n != 2 {
		t.Fatalf("concurrent limit = %d, want 2", n)
	}
	a.SetSubagentMaxConcurrent(0)
	if n := a.SubagentMaxConcurrent(); n != config.DefaultSubagentMaxConcurrent {
		t.Fatalf("zero concurrent limit should fall back to default, got %d", n)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 1000; i++ {
			_ = a.BoardEnabled()
			_ = a.SubagentsEnabled()
			_ = a.SubagentMaxDepth()
			_ = a.SubagentMaxConcurrent()
		}
	}()
	for i := 0; i < 100; i++ {
		a.SetBoardEnabled(i%2 == 0)
	}
	<-done
}

// TestSessionAgentFactorySeedsFlags verifies NewSessionAgent propagates the
// live feature flags onto the created agent.
func TestSessionAgentFactorySeedsFlags(t *testing.T) {
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	opts := SessionAgentOptions{
		Provider:              prov,
		Executor:              exec,
		Config:                &config.Config{ContextLimit: 128000},
		WorkingDir:            exec.GetWorkingDir(),
		BoardEnabled:          true,
		SubagentsEnabled:      true,
		SubagentMaxDepth:      4,
		SubagentMaxConcurrent: 7,
	}
	a := NewSessionAgent(opts, nil, "test-id")
	if !a.BoardEnabled() || !a.SubagentsEnabled() {
		t.Fatalf("factory did not seed flags: board=%v subagents=%v", a.BoardEnabled(), a.SubagentsEnabled())
	}
	if d := a.SubagentMaxDepth(); d != 4 {
		t.Fatalf("factory depth = %d, want 4", d)
	}
	if n := a.SubagentMaxConcurrent(); n != 7 {
		t.Fatalf("factory concurrent limit = %d, want 7", n)
	}
	if a.SessionID != "test-id" {
		t.Fatalf("session id = %q, want test-id", a.SessionID)
	}
}

// TestSharedFeatureFlagsStore verifies the shared-reader contract: agents
// pointed at one FeatureFlags store see every write — from the store (the
// workspace toggle) or from any other agent — immediately, with no mirror
// and no sweep.
func TestSharedFeatureFlagsStore(t *testing.T) {
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	a1 := NewAgent(prov, exec, nil)
	a2 := NewAgent(prov, exec, nil)
	shared := NewFeatureFlags(false, false, 0, 0)
	a1.SetFeatureFlags(shared)
	a2.SetFeatureFlags(shared)

	// A workspace-side write is visible to every attached agent at once.
	shared.SetBoardEnabled(true)
	shared.SetSubagentsEnabled(true)
	shared.SetSubagentMaxDepth(3)
	shared.SetSubagentMaxConcurrent(2)
	if !a1.BoardEnabled() || !a2.BoardEnabled() {
		t.Fatalf("shared board toggle not visible: a1=%v a2=%v", a1.BoardEnabled(), a2.BoardEnabled())
	}
	if !a1.SubagentsEnabled() || !a2.SubagentsEnabled() {
		t.Fatalf("shared subagent toggle not visible: a1=%v a2=%v", a1.SubagentsEnabled(), a2.SubagentsEnabled())
	}
	if a1.SubagentMaxDepth() != 3 || a2.SubagentMaxDepth() != 3 {
		t.Fatalf("shared depth not visible: a1=%d a2=%d", a1.SubagentMaxDepth(), a2.SubagentMaxDepth())
	}
	if a1.SubagentMaxConcurrent() != 2 || a2.SubagentMaxConcurrent() != 2 {
		t.Fatalf("shared limit not visible: a1=%d a2=%d", a1.SubagentMaxConcurrent(), a2.SubagentMaxConcurrent())
	}

	// An agent-side write through the shared store reaches the other agent.
	a1.SetBoardEnabled(false)
	if a2.BoardEnabled() {
		t.Fatal("agent-side write did not reach the other agent")
	}

	// Detaching falls back to a private store: further shared writes are
	// invisible to the detached agent.
	a1.SetFeatureFlags(nil)
	shared.SetBoardEnabled(true)
	if a1.BoardEnabled() {
		t.Fatal("detached agent still reads the shared store")
	}
	if !a2.BoardEnabled() {
		t.Fatal("attached agent lost the shared store")
	}
}

// TestSessionAgentFactorySharedFlags verifies NewSessionAgent prefers the
// shared FeatureFlags store over the per-value fields: the created agent
// reads the store directly, so a later store write is visible without any
// re-seed.
func TestSessionAgentFactorySharedFlags(t *testing.T) {
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	shared := NewFeatureFlags(true, false, 4, 7)
	opts := SessionAgentOptions{
		Provider:     prov,
		Executor:     exec,
		WorkingDir:   exec.GetWorkingDir(),
		FeatureFlags: shared,
		// Value fields must be ignored when the shared store is set.
		BoardEnabled:          false,
		SubagentsEnabled:      true,
		SubagentMaxDepth:      1,
		SubagentMaxConcurrent: 1,
	}
	a := NewSessionAgent(opts, nil, "shared-id")
	if a.FeatureFlags() != shared {
		t.Fatal("factory did not attach the shared store")
	}
	if !a.BoardEnabled() || a.SubagentsEnabled() {
		t.Fatalf("agent did not read the shared store: board=%v subagents=%v", a.BoardEnabled(), a.SubagentsEnabled())
	}
	if a.SubagentMaxDepth() != 4 || a.SubagentMaxConcurrent() != 7 {
		t.Fatalf("agent did not read the shared store: depth=%d limit=%d", a.SubagentMaxDepth(), a.SubagentMaxConcurrent())
	}
	shared.SetSubagentsEnabled(true)
	if !a.SubagentsEnabled() {
		t.Fatal("later shared write not visible to the factory-created agent")
	}
}

// TestSharedFlagStoreGatesOptionalTools pins the WEB contract for the
// optional-tool flags: the settings toggle writes only the shared store, so
// an already-built session agent must pick the change up (no per-agent
// setter, no sweep) and the tool must appear in llmTools/AllowedToolNames —
// which is what featureTools feeds.
func TestSharedFlagStoreGatesOptionalTools(t *testing.T) {
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	a := NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
	shared := NewFeatureFlags(false, false, 0, 0)
	a.SetFeatureFlags(shared)

	has := func(name string) bool {
		if _, ok := a.AllowedToolNames()[name]; !ok {
			return false
		}
		for _, d := range a.llmTools() {
			if d.Name == name {
				return true
			}
		}
		return false
	}

	if has("terminal") || has("bash_persistent") {
		t.Fatal("terminal tools must be off by default")
	}
	shared.SetTerminalEnabled(true)
	if !has("terminal") || !has("bash_persistent") {
		t.Fatal("a shared-store terminal toggle must reach the agent's tool set")
	}
	shared.SetTerminalEnabled(false)
	if has("terminal") {
		t.Fatal("turning the shared terminal flag off must remove the tool")
	}

	// Skills additionally requires an installed manager, so the flag alone
	// must not register the tool.
	shared.SetSkillsEnabled(true)
	if has("skill") {
		t.Fatal("the skill tool must not register without a skills manager")
	}
	a.SetSkillsManager(skills.NewManager(t.TempDir(), false))
	if !has("skill") {
		t.Fatal("a shared-store skills toggle plus a manager must register the skill tool")
	}
	shared.SetSkillsEnabled(false)
	if has("skill") {
		t.Fatal("turning the shared skills flag off must remove the tool")
	}
}
