package tui

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/server"
	"gogen/internal/session"
)

// spawnTUISubagent runs one TUI subagent spawn over mock providers and
// returns the child provider (for model assertions), the report, and the
// spawn error. The parent is a bare agent on a mock provider running
// "parent-model"; the child provider is seeded with "default-model" (the
// cfg.OpenAIModel analog of the web workspace default) and serves a canned
// report.
func spawnTUISubagent(t *testing.T, cfg *config.Config, childModels []llm.ModelInfo, job, model string) (*llmtest.MockProvider, string, error) {
	t.Helper()
	parentProv := llmtest.NewMockProvider()
	if err := parentProv.SetModel("parent-model"); err != nil {
		t.Fatal(err)
	}
	exec := agent.NewExecutor(t.TempDir())
	parent := agent.NewAgent(parentProv, exec, contextmgr.NewManager(parentProv, contextmgr.Settings{ContextLimit: 128000}))
	parent.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})

	var child *llmtest.MockProvider
	sp := newTUISubagentSpawner(cfg, nil)
	sp.providerFactory = func(_ *config.Config, _ *agent.Agent) llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = childModels
		p.StreamResults = []*llm.StreamResult{{Content: "report"}}
		child = p
		return p
	}
	report, err := sp.Spawn(context.Background(), parent, job, model, 0)
	return child, report, err
}

// assertChildModel checks the child provider's final model.
func assertChildModel(t *testing.T, child *llmtest.MockProvider, want string) {
	t.Helper()
	if child == nil {
		t.Fatal("child provider was never created")
	}
	if got := child.ModelName(); got != want {
		t.Fatalf("child model = %q, want %q", got, want)
	}
}

// TestTUISubagentModelCascade pins the TUI spawner's model cascade: it must
// match the web spawner (shared ResolveSubagentModel) — explicit tool
// argument > configured subagent model > the parent's live model — with
// the winner validated against the child's own catalog (SelectModel).
func TestTUISubagentModelCascade(t *testing.T) {
	fullCatalog := []llm.ModelInfo{
		{ID: "default-model", ContextLimit: 128000},
		{ID: "explicit-model", ContextLimit: 128000},
		{ID: "configured-model", ContextLimit: 128000},
		{ID: "parent-model", ContextLimit: 128000},
	}
	cases := []struct {
		name  string
		cfg   *config.Config
		model string
		want  string
	}{
		{
			name:  "explicit wins over configured and parent",
			cfg:   &config.Config{OpenAIModel: "default-model", SubagentModel: "configured-model"},
			model: "explicit-model",
			want:  "explicit-model",
		},
		{
			name: "configured wins over parent",
			cfg:  &config.Config{OpenAIModel: "default-model", SubagentModel: "configured-model"},
			want: "configured-model",
		},
		{
			name: "parent inherits when it differs from the default",
			cfg:  &config.Config{OpenAIModel: "default-model"},
			want: "parent-model",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			child, report, err := spawnTUISubagent(t, tc.cfg, fullCatalog, "job", tc.model)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(report, "report") {
				t.Fatalf("report = %q", report)
			}
			assertChildModel(t, child, tc.want)
		})
	}
}

// TestTUISubagentReportBounded verifies the TUI shares the documented
// subagent report cap, so an oversized foreground child cannot bypass it via
// a host-specific spawner.
func TestTUISubagentReportBounded(t *testing.T) {
	cfg := &config.Config{OpenAIModel: "default-model"}
	models := []llm.ModelInfo{{ID: "default-model", ContextLimit: 128000}}
	parentProv := llmtest.NewMockProvider()
	parent := agent.NewAgent(parentProv, agent.NewExecutor(t.TempDir()), contextmgr.NewManager(parentProv, contextmgr.Settings{ContextLimit: 128000}))
	sp := newTUISubagentSpawner(cfg, nil)
	sp.providerFactory = func(*config.Config, *agent.Agent) llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = models
		p.StreamResults = []*llm.StreamResult{{Content: strings.Repeat("x", agent.MaxSubagentReportBytes+1000)}}
		return p
	}
	report, err := sp.Spawn(t.Context(), parent, "job", "", 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(report, "… (truncated)") {
		t.Fatalf("oversized TUI subagent report was not truncated")
	}
}

// TestTUISubagentExplicitModelFailsSpawn pins the shared spawner's hard
// requirement: an explicit model that is not in the child's catalog fails
// the spawn — the TUI must not silently run an unrelated model.
func TestTUISubagentExplicitModelFailsSpawn(t *testing.T) {
	child, _, err := spawnTUISubagent(t, &config.Config{OpenAIModel: "default-model"},
		[]llm.ModelInfo{{ID: "default-model", ContextLimit: 128000}}, "job", "no/such-model")
	if err == nil || !strings.Contains(err.Error(), "subagent model") {
		t.Fatalf("spawn error = %v, want a subagent model error", err)
	}
	// The child provider is created before selection; the turn must not
	// have run.
	if child != nil && child.CallCount != 0 {
		t.Fatalf("child provider was used despite the failed selection (calls = %d)", child.CallCount)
	}
}

// TestTUISubagentConfiguredModelFallback pins the fail-open tier: a
// configured subagent model that is no longer in the child's catalog falls
// back to the seeded default instead of failing the spawn.
func TestTUISubagentConfiguredModelFallback(t *testing.T) {
	child, _, err := spawnTUISubagent(t,
		&config.Config{OpenAIModel: "default-model", SubagentModel: "vanished-model"},
		[]llm.ModelInfo{{ID: "default-model", ContextLimit: 128000}}, "job", "")
	if err != nil {
		t.Fatal(err)
	}
	assertChildModel(t, child, "default-model")
}

// TestTUISubagentParentModelFallback pins the fail-open inheritance tier:
// a parent model that is no longer in the child's catalog falls back to
// the seeded default.
func TestTUISubagentParentModelFallback(t *testing.T) {
	child, _, err := spawnTUISubagent(t, &config.Config{OpenAIModel: "default-model"},
		[]llm.ModelInfo{{ID: "default-model", ContextLimit: 128000}}, "job", "")
	if err != nil {
		t.Fatal(err)
	}
	assertChildModel(t, child, "default-model")
}

// TestTUISpawnerBuildsChildrenThroughWorkspace pins that a workspace-attached
// spawner builds its children with the SHARED lifecycle factory (web parity)
// instead of the hand-built fallback: the child reads the workspace's live
// feature flags, inherits the workspace's spawner, and the fallback provider
// factory never runs.
func TestTUISpawnerBuildsChildrenThroughWorkspace(t *testing.T) {
	parentProv := llmtest.NewMockProvider()
	parentProv.Model = "default-model"
	exec := agent.NewExecutor(t.TempDir())
	parent := agent.NewAgent(parentProv, exec, contextmgr.NewManager(parentProv, contextmgr.Settings{ContextLimit: 128000}))
	parent.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
	cfg := &config.Config{OpenAIModel: "default-model", Subagent: "on", SubagentMaxDepth: 1}
	ws := server.NewWorkspaceForHost(parent, cfg)

	sp := newTUISubagentSpawner(cfg, ws)
	sp.providerFactory = func(*config.Config, *agent.Agent) llm.LLMProvider {
		t.Fatal("the hand-built child provider ran despite an attached workspace")
		return nil
	}
	ws.SubagentSpawner = sp
	// A live flag change marks the workspace: a workspace-built child reads
	// it, while the hand-built path would have inherited the parent's value.
	ws.SetSubagentMaxDepth(3)

	child := sp.newChildAgent(parent)
	if child == nil {
		t.Fatal("no child agent")
	}
	if child.SessionID == "" || child.SessionID == parent.SessionID {
		t.Fatalf("child session id = %q, want a fresh id", child.SessionID)
	}
	if got := child.SubagentMaxDepth(); got != 3 {
		t.Fatalf("child SubagentMaxDepth = %d, want the workspace value 3 (child not built through the workspace)", got)
	}
	if got := child.SubagentSpawner(); got != agent.SubagentSpawner(sp) {
		t.Fatalf("child spawner = %T, want the host's single spawner", got)
	}
}

// TestNewWithWorkspaceInstallsOneSpawner pins the host wiring: with the
// subagent feature on, the runner installs ONE spawner on the workspace and on
// the root agent, and every session the workspace creates inherits that same
// instance (so the continuable child registry cannot be duplicated per
// session). With the feature off, no spawner is installed at all.
func TestNewWithWorkspaceInstallsOneSpawner(t *testing.T) {
	build := func(t *testing.T, cfg *config.Config) (*agent.Agent, *server.Workspace) {
		t.Helper()
		prov := llmtest.NewMockProvider()
		prov.Model = "default-model"
		a := agent.NewAgent(prov, agent.NewExecutor(t.TempDir()), contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
		a.SessionStore = session.NewStoreWithOptions(true, session.StoreOptions{})
		return a, server.NewWorkspaceForHost(a, cfg)
	}

	t.Run("enabled", func(t *testing.T) {
		cfg := &config.Config{OpenAIModel: "default-model", Subagent: "on"}
		a, ws := build(t, cfg)
		ui := NewWithWorkspace(a, cfg, ws)
		if ui.spawner == nil {
			t.Fatal("runner built no spawner")
		}
		if ws.SubagentSpawner != ui.spawner {
			t.Fatal("workspace must carry the runner's spawner")
		}
		if got := a.SubagentSpawner(); got != agent.SubagentSpawner(ui.spawner) {
			t.Fatalf("root agent spawner = %T, want the runner's", got)
		}
		opened := ws.NewSessionAgent(nil, session.NewID())
		if got := opened.SubagentSpawner(); got != agent.SubagentSpawner(ui.spawner) {
			t.Fatalf("workspace-created session spawner = %T, want the runner's", got)
		}
		// The Model reuses that instance (one registry) and attaches its
		// /subagents list to it.
		m := NewModel(a, cfg)
		m.spawner = ui.spawner
		m.installSubagentSpawner()
		if m.spawner != ui.spawner {
			t.Fatal("Model built a second spawner instead of reusing the host's")
		}
		if ui.spawner.record == nil {
			t.Fatal("Model did not attach its /subagents list to the host spawner")
		}
	})

	t.Run("disabled", func(t *testing.T) {
		cfg := &config.Config{OpenAIModel: "default-model"}
		a, ws := build(t, cfg)
		ui := NewWithWorkspace(a, cfg, ws)
		if ui.spawner != nil {
			t.Fatal("subagents are off: the runner must not build a spawner")
		}
		if ws.SubagentSpawner != nil || a.SubagentSpawner() != nil {
			t.Fatal("subagents are off: no spawner may be installed")
		}
	})
}
