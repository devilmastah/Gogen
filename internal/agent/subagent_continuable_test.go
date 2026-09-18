package agent

import (
	"context"
	"strings"
	"testing"

	"gogen/internal/llm"
)

// fakeContinuableSpawner is a fakeSpawner that also implements
// ContinuableSubagentSpawner.
type fakeContinuableSpawner struct {
	fakeSpawner
	bgCalls int
}

func (f *fakeContinuableSpawner) SpawnBackground(ctx context.Context, parent *Agent, job, model string, depth int) (string, error) {
	f.bgCalls++
	return "bg-child-1", nil
}

func (f *fakeContinuableSpawner) Fork(ctx context.Context, parent *Agent, job string, depth int) (string, error) {
	return "fork report", nil
}

func (f *fakeContinuableSpawner) ListAgents(caller *Agent) (string, error) {
	return "bg-child-1 — subagent: test — finished (depth 1)", nil
}

func (f *fakeContinuableSpawner) SendMessage(caller *Agent, agentID, text string) error { return nil }
func (f *fakeContinuableSpawner) InterruptAgent(caller *Agent, agentID string) error    { return nil }

// subagentToolNames returns the feature-tool names in a's model-visible list.
func subagentToolNames(a *Agent) map[string]bool {
	out := map[string]bool{}
	for _, def := range a.llmTools() {
		switch def.Name {
		case "subagent", "subagent_fork", "list_agents", "send_message", "interrupt_agent":
			out[def.Name] = true
		}
	}
	return out
}

// TestContinuableToolGating pins the continuable surface: with a plain
// spawner the subagent tool stays foreground-only and none of the
// continuable tools exist; with a continuable spawner the parameter and the
// control tools appear. There is no child-scoped progress tool: a child's
// one return channel is its final message (the parent pulls progress via
// list_agents).
func TestContinuableToolGating(t *testing.T) {
	a := newSubagentTestAgent(t)
	a.SetSubagentsEnabled(true)

	// Plain spawner: foreground schema only.
	a.SetSubagentSpawner(&fakeSpawner{report: "r"})
	defs := subagentToolNames(a)
	if !defs["subagent"] || defs["subagent_fork"] || defs["list_agents"] || defs["send_message"] || defs["interrupt_agent"] {
		t.Fatalf("plain spawner surface = %v", defs)
	}
	for _, def := range a.llmTools() {
		if def.Name == "subagent" {
			if _, has := def.Parameters["properties"].(map[string]any)["run_in_background"]; has {
				t.Fatal("run_in_background must not appear with a plain spawner")
			}
		}
	}

	// Continuable spawner: parameter + control tools.
	cs := &fakeContinuableSpawner{fakeSpawner: fakeSpawner{report: "r"}}
	a.SetSubagentSpawner(cs)
	defs = subagentToolNames(a)
	for _, name := range []string{"subagent", "subagent_fork", "list_agents", "send_message", "interrupt_agent"} {
		if !defs[name] {
			t.Fatalf("continuable surface missing %s: %v", name, defs)
		}
	}
	seen := false
	for _, def := range a.llmTools() {
		if def.Name == "subagent" {
			seen = true
			props := def.Parameters["properties"].(map[string]any)
			if _, has := props["run_in_background"]; !has {
				t.Fatal("run_in_background must appear with a continuable spawner")
			}
		}
	}
	if !seen {
		t.Fatal("subagent tool missing")
	}
}

// TestContinuableToolExecution routes the new tools through executeTool and
// verifies run_in_background picks the background path.
func TestContinuableToolExecution(t *testing.T) {
	a := newSubagentTestAgent(t)
	a.SetSubagentsEnabled(true)
	cs := &fakeContinuableSpawner{fakeSpawner: fakeSpawner{report: "r"}}
	a.SetSubagentSpawner(cs)

	out, err := a.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]any{"job": "j", "run_in_background": true}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bg-child-1") {
		t.Fatalf("background spawn output = %q", out)
	}
	if cs.bgCalls != 1 {
		t.Fatalf("SpawnBackground calls = %d, want 1", cs.bgCalls)
	}

	if out, err = a.executeTool(t.Context(), llm.ToolCall{Name: "subagent", Args: map[string]any{"job": "j"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "r") {
		t.Fatalf("foreground spawn output = %q", out)
	}
	if cs.bgCalls != 1 {
		t.Fatalf("foreground call must not use SpawnBackground")
	}

	if out, err = a.executeTool(t.Context(), llm.ToolCall{Name: "subagent_fork", Args: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "fork report") {
		t.Fatalf("fork output = %q", out)
	}
	if out, err = a.executeTool(t.Context(), llm.ToolCall{Name: "list_agents", Args: map[string]any{}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "bg-child-1") {
		t.Fatalf("list output = %q", out)
	}
	if out, err = a.executeTool(t.Context(), llm.ToolCall{Name: "send_message", Args: map[string]any{"agent_id": "bg-child-1", "message": "hi"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "delivered") {
		t.Fatalf("send output = %q", out)
	}
	if out, err = a.executeTool(t.Context(), llm.ToolCall{Name: "interrupt_agent", Args: map[string]any{"agent_id": "bg-child-1"}}); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Interrupted") {
		t.Fatalf("interrupt output = %q", out)
	}

	// Continuable family is act-only.
	a.SetMode(ModePlan)
	for _, name := range []string{"subagent", "subagent_fork", "list_agents", "send_message", "interrupt_agent"} {
		if _, ok := a.AllowedToolNames()[name]; ok {
			t.Fatalf("%s must be blocked in plan mode", name)
		}
		if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: name, Args: map[string]any{}}); err == nil {
			t.Fatalf("%s must fail in plan mode", name)
		}
	}
}
