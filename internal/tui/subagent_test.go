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
	sp := &tuiSubagentSpawner{
		cfg: cfg,
		providerFactory: func(_ *config.Config, _ *agent.Agent) llm.LLMProvider {
			p := llmtest.NewMockProvider()
			p.Model = "default-model"
			p.Models = childModels
			p.StreamResults = []*llm.StreamResult{{Content: "report"}}
			child = p
			return p
		},
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
	sp := &tuiSubagentSpawner{cfg: cfg, providerFactory: func(*config.Config, *agent.Agent) llm.LLMProvider {
		p := llmtest.NewMockProvider()
		p.Model = "default-model"
		p.Models = models
		p.StreamResults = []*llm.StreamResult{{Content: strings.Repeat("x", agent.MaxSubagentReportBytes+1000)}}
		return p
	}}
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
