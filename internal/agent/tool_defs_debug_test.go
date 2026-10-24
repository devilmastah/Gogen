package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/debuglog"
	"gogen/internal/llm"
)

// TestToolDefinitionsDebugLog pins the diagnostic that settles "the model says
// its tool list had no <tool>" reports: with debug logging enabled, every
// round records the exact tool names and the session mode the request carried.
// A model's self-report is not evidence (models mis-state and even invent tool
// names — e.g. the mcp_list/mcp_get/mcp_run names that exist nowhere in this
// codebase), so the client-side record has to answer it.
func TestToolDefinitionsDebugLog(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "debug.log")
	debuglog.Configure(logPath, "tools-log-test")
	t.Cleanup(func() { debuglog.Configure("", "") })

	provider := &statsStubProvider{limit: 1000, streamResult: &llm.StreamResult{Content: "done"}}
	a := NewAgent(provider, &Executor{WorkingDir: "."}, nil)
	if _, err := a.StreamProcessInput(context.Background(), "write a file", nil); err != nil {
		t.Fatalf("turn: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	log := string(raw)
	if !strings.Contains(log, `"location":"agent/tools"`) {
		t.Fatalf("no agent/tools entry in the debug log:\n%s", log)
	}
	// Act mode sends every builtin — the write tools included, which is the
	// exact claim being disproved.
	for _, want := range []string{`"mode":"act"`, `"write_file"`, `"patch_file"`, `"execute_command"`} {
		if !strings.Contains(log, want) {
			t.Errorf("debug log missing %s:\n%s", want, log)
		}
	}
}

// TestToolDefinitionsDebugLogPlanMode pins the other half: the same diagnostic
// shows the reduced plan-mode set, so a genuinely filtered tool list (the real
// cause when mode is plan) is visible too.
func TestToolDefinitionsDebugLogPlanMode(t *testing.T) {
	logPath := filepath.Join(t.TempDir(), "debug.log")
	debuglog.Configure(logPath, "tools-log-plan")
	t.Cleanup(func() { debuglog.Configure("", "") })

	provider := &statsStubProvider{limit: 1000, streamResult: &llm.StreamResult{Content: "done"}}
	a := NewAgent(provider, &Executor{WorkingDir: "."}, nil)
	a.Mode = ModePlan
	if _, err := a.StreamProcessInput(context.Background(), "look around", nil); err != nil {
		t.Fatalf("turn: %v", err)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("read debug log: %v", err)
	}
	log := string(raw)
	if !strings.Contains(log, `"mode":"plan"`) {
		t.Fatalf("plan-mode entry missing from the debug log:\n%s", log)
	}
	if strings.Contains(log, `"write_file"`) {
		t.Errorf("plan mode must not log write_file as sent:\n%s", log)
	}
}
