package agent

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
)

// fakeMCPRegistry is a minimal MCPToolRegistry for eligibility tests.
type fakeMCPRegistry struct {
	names map[string]struct{}
	defs  []llm.Tool // optional definitions (tool-collision tests)
}

func (f *fakeMCPRegistry) Definitions() []llm.Tool        { return f.defs }
func (f *fakeMCPRegistry) ToolNames() map[string]struct{} { return f.names }
func (f *fakeMCPRegistry) CallTool(context.Context, string, map[string]any) (string, error) {
	return "", nil
}

// TestToolCallsParallelEligible verifies the batch classifier: only batches
// of 2+ builtin read-only tools are eligible, and mutating or MCP-shadowed
// names force the sequential path.
func TestToolCallsParallelEligible(t *testing.T) {
	a := &Agent{}
	readOnly := []llm.ToolCall{
		{ID: "c1", Name: "read_file"},
		{ID: "c2", Name: "search_code"},
	}
	if !a.toolCallsParallelEligible(readOnly) {
		t.Fatal("two read-only calls should be eligible")
	}
	if a.toolCallsParallelEligible(readOnly[:1]) {
		t.Fatal("a single call should not be parallel")
	}
	mixed := []llm.ToolCall{
		{ID: "c1", Name: "read_file"},
		{ID: "c3", Name: "write_file"},
	}
	if a.toolCallsParallelEligible(mixed) {
		t.Fatal("a batch with a mutating tool must run sequentially")
	}
	// MCP-shadowed names are excluded even when every name is a builtin.
	a.SetMCPRegistry(&fakeMCPRegistry{names: map[string]struct{}{"read_file": {}}})
	if a.toolCallsParallelEligible(readOnly) {
		t.Fatal("an MCP-shadowed name must force the sequential path")
	}
}

// TestExecuteToolCallsParallelStreamsToolOutput verifies that the parallel
// branch attaches the live-output sink to each call's context (exactly like
// the sequential runToolRound path), so execute_command calls in a parallel
// batch stream intermediate chunks to the OnToolOutput handler tagged with
// the right call identity (id/name). The batch is fed directly to
// executeToolCallsParallel because execute_command is a mutating tool and
// never passes the parallel-eligibility classifier (covered separately).
func TestExecuteToolCallsParallelStreamsToolOutput(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(llmtest.NewMockProvider(), exec, nil)

	// Each command prints in multiple chunks; every Write from the child
	// process is forwarded to the sink, so concatenated chunks per call
	// must equal that command's full output.
	cmds := []string{
		"printf 'first\\n'; printf 'second\\n'",
		"printf 'alpha\\n'; printf 'beta\\n'",
	}
	toolCalls := []llm.ToolCall{
		{ID: "c1", Name: "execute_command", Args: map[string]any{"command": cmds[0]}},
		{ID: "c2", Name: "execute_command", Args: map[string]any{"command": cmds[1]}},
	}

	var mu sync.Mutex
	type streamed struct {
		id, name, command, chunk string
	}
	var got []streamed
	results := 0
	h := &llm.StreamHandlers{
		OnToolOutput: func(id, name, command, chunk string) {
			mu.Lock()
			defer mu.Unlock()
			got = append(got, streamed{id, name, command, chunk})
		},
		OnToolResult: func(id, name, result string, success bool) {
			mu.Lock()
			defer mu.Unlock()
			results++
			if !success {
				t.Errorf("expected success for %s, got result %q", name, result)
			}
		},
	}

	if cancelled := a.executeToolCallsParallel(context.Background(), h, toolCalls); cancelled {
		t.Fatal("unexpected cancellation")
	}

	mu.Lock()
	defer mu.Unlock()
	if results != 2 {
		t.Fatalf("expected 2 tool results, got %d", results)
	}
	byID := map[string][]string{}
	commands := map[string]string{}
	for _, s := range got {
		if s.name != "execute_command" {
			t.Fatalf("unexpected tool name %q for call %s", s.name, s.id)
		}
		if commands[s.id] != "" && commands[s.id] != s.command {
			t.Fatalf("call %s streamed chunks for commands %q and %q", s.id, commands[s.id], s.command)
		}
		commands[s.id] = s.command
		byID[s.id] = append(byID[s.id], s.chunk)
	}
	want := map[string]string{
		"c1": "first\nsecond\n",
		"c2": "alpha\nbeta\n",
	}
	wantCmd := map[string]string{"c1": cmds[0], "c2": cmds[1]}
	for id, out := range want {
		if commands[id] != wantCmd[id] {
			t.Fatalf("call %s streamed with command %q, want %q", id, commands[id], wantCmd[id])
		}
		if joined := strings.Join(byID[id], ""); joined != out {
			t.Fatalf("call %s streamed %q, want %q", id, joined, out)
		}
	}
}

// TestExecuteToolCallsParallelRunsReadOnlyToolsConcurrently verifies that a
// batch of read-only tool calls is executed concurrently (observed overlap),
// that every tool_call gets a matching tool result, and that results are
// appended in the model's call order.
func TestExecuteToolCallsParallelRunsReadOnlyToolsConcurrently(t *testing.T) {
	dir := t.TempDir()
	files := []string{"a.txt", "b.txt", "c.txt"}
	for i, name := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("content-"+string(rune('a'+i))), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	prov := llmtest.NewMockProvider()
	prov.StreamResults = []*llm.StreamResult{
		{
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "read_file", Args: map[string]any{"path": "a.txt"}},
				{ID: "c2", Name: "read_file", Args: map[string]any{"path": "b.txt"}},
				{ID: "c3", Name: "read_file", Args: map[string]any{"path": "c.txt"}},
			},
		},
		{Content: "done"},
	}
	exec := NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000})
	a := NewAgent(prov, exec, ctxMgr)

	// Wrap read_file with an overlap detector: record the max number of
	// concurrent executions. A sequential runner would never exceed 1.
	var mu sync.Mutex
	active, maxActive := 0, 0
	builtin := BuiltinToolHandlers()
	orig := builtin["read_file"]
	builtin["read_file"] = func(ctx context.Context, a *Agent, args map[string]any) (string, error) {
		mu.Lock()
		active++
		if active > maxActive {
			maxActive = active
		}
		mu.Unlock()
		defer func() {
			mu.Lock()
			active--
			mu.Unlock()
		}()
		time.Sleep(30 * time.Millisecond)
		return orig(ctx, a, args)
	}
	a.SetToolHandlers(builtin)

	if _, err := a.StreamProcessInput(context.Background(), "read three files", nil); err != nil {
		t.Fatalf("StreamProcessInput: %v", err)
	}
	if maxActive < 2 {
		t.Fatalf("expected concurrent read_file executions (maxActive=%d), tools did not run in parallel", maxActive)
	}

	// Every tool_call must have a matching tool result, in call order.
	var toolOrder []string
	results := map[string]string{}
	for _, m := range a.Messages {
		if m.Role == "assistant" {
			for _, tc := range m.ToolCalls {
				toolOrder = append(toolOrder, tc.ID)
			}
		}
		if m.Role == "tool" && m.ToolCallID != "" {
			results[m.ToolCallID] = m.Content
		}
	}
	want := []string{"c1", "c2", "c3"}
	if len(toolOrder) != len(want) {
		t.Fatalf("tool call order = %v, want %v", toolOrder, want)
	}
	for i := range want {
		if toolOrder[i] != want[i] {
			t.Fatalf("tool call order = %v, want %v", toolOrder, want)
		}
	}
	for i, id := range want {
		got := results[id]
		if got == "" {
			t.Fatalf("tool call %s has no result", id)
		}
		if !strings.Contains(got, "content-"+string(rune('a'+i))) {
			t.Fatalf("tool call %s result does not contain file %d content: %q", id, i, got)
		}
	}
}

// TestToolCallsParallelCancelStillClosesEveryToolCall verifies the cancel
// path: when the turn context is cancelled mid-parallel-execution, every
// tool_call still receives a matching tool result so the next turn's
// tool-call/result protocol stays valid.
func TestToolCallsParallelCancelStillClosesEveryToolCall(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	prov := llmtest.NewMockProvider()
	prov.StreamResults = []*llm.StreamResult{
		{
			ToolCalls: []llm.ToolCall{
				{ID: "c1", Name: "read_file", Args: map[string]any{"path": "a.txt"}},
				{ID: "c2", Name: "read_file", Args: map[string]any{"path": "a.txt"}},
			},
		},
		{Content: "done"},
	}
	exec := NewExecutor(dir)
	ctxMgr := contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000})
	a := NewAgent(prov, exec, ctxMgr)

	builtin := BuiltinToolHandlers()
	orig := builtin["read_file"]
	// entered is signalled by each call that starts executing. The cancel must
	// wait for BOTH: a fixed sleep raced a loaded (or -race) runner, and a
	// cancel that lands before the tool round begins means the round never
	// runs — no assistant tool_calls message and no results at all, which is
	// protocol-clean but is NOT what this test asserts (it reported the
	// misleading "tool call c1 missing a result after cancel").
	entered := make(chan struct{}, 2) // one slot per call in this batch
	builtin["read_file"] = func(ctx context.Context, a *Agent, args map[string]any) (string, error) {
		select {
		case entered <- struct{}{}:
		default: // more calls than the buffer: never block the tool
		}
		// Block long enough for the cancel to land mid-flight.
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-time.After(5 * time.Second):
		}
		return orig(ctx, a, args)
	}
	a.SetToolHandlers(builtin)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		_, _ = a.StreamProcessInput(ctx, "read", nil)
	}()
	for i := 0; i < 2; i++ {
		select {
		case <-entered:
		case <-time.After(30 * time.Second):
			cancel()
			<-done
			t.Fatalf("only %d of 2 parallel tool calls started before the wait timed out", i)
		}
	}
	cancel()
	<-done

	// The assistant message's tool calls must all have results.
	found := map[string]bool{}
	for _, m := range a.Messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			found[m.ToolCallID] = true
		}
	}
	for _, id := range []string{"c1", "c2"} {
		if !found[id] {
			t.Fatalf("tool call %s missing a result after cancel", id)
		}
	}
}

// TestExecuteToolCallsParallelEntryCancelClosesBatch pins the entry-cancel
// contract: when the turn context is already cancelled before the batch
// starts, every tool call still receives a placeholder result. The caller
// already appended the assistant tool_calls message, so returning an open
// batch would make the next request fail provider-side ("insufficient tool
// messages following tool_calls message").
func TestExecuteToolCallsParallelEntryCancelClosesBatch(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(llmtest.NewMockProvider(), exec, nil)
	toolCalls := []llm.ToolCall{
		{ID: "c1", Name: "read_file", Args: map[string]any{"path": "a.txt"}},
		{ID: "c2", Name: "read_file", Args: map[string]any{"path": "a.txt"}},
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !a.executeToolCallsParallel(ctx, &llm.StreamHandlers{}, toolCalls) {
		t.Fatal("expected the pre-cancelled batch to report cancellation")
	}

	found := map[string]bool{}
	for _, m := range a.Messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			found[m.ToolCallID] = true
		}
	}
	for _, id := range []string{"c1", "c2"} {
		if !found[id] {
			t.Fatalf("tool call %s missing a result after the entry cancel", id)
		}
	}
}

// TestExecuteToolCallsParallelCancelKeepsCompletedResults verifies that when
// the turn context is cancelled right after a parallel tool has returned,
// that tool's completed result is preserved (real output, success=true)
// instead of being overwritten with the cancelled placeholder. Only the
// still-in-flight call reads as cancelled — mirroring runToolRound, where a
// tool that already finished keeps its output.
func TestExecuteToolCallsParallelCancelKeepsCompletedResults(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "fast.txt"), []byte("fast-result"), 0o644); err != nil {
		t.Fatal(err)
	}

	exec := NewExecutor(dir)
	a := NewAgent(llmtest.NewMockProvider(), exec, nil)

	builtin := BuiltinToolHandlers()
	orig := builtin["read_file"]
	fastDone := make(chan struct{})
	var once sync.Once
	builtin["read_file"] = func(ctx context.Context, a *Agent, args map[string]any) (string, error) {
		if path, _ := args["path"].(string); path == "slow.txt" {
			// Stay in flight until the test cancels the turn context.
			<-ctx.Done()
			return "", ctx.Err()
		}
		res, err := orig(ctx, a, args)
		once.Do(func() { close(fastDone) })
		return res, err
	}
	a.SetToolHandlers(builtin)

	toolCalls := []llm.ToolCall{
		{ID: "c1", Name: "read_file", Args: map[string]any{"path": "fast.txt"}},
		{ID: "c2", Name: "read_file", Args: map[string]any{"path": "slow.txt"}},
	}

	var mu sync.Mutex
	onResult := map[string]bool{}
	h := &llm.StreamHandlers{
		OnToolResult: func(id, name, result string, success bool) {
			mu.Lock()
			defer mu.Unlock()
			onResult[id] = success
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan bool, 1)
	go func() {
		done <- a.executeToolCallsParallel(ctx, h, toolCalls)
	}()
	<-fastDone // c1 has returned its real result...
	cancel()   // ...cancel right after, while c2 is still in flight.
	if !<-done {
		t.Fatal("expected the parallel batch to report cancellation")
	}

	// The completed call keeps its real output; the interrupted call reads
	// as cancelled.
	persisted := map[string]string{}
	for _, m := range a.Messages {
		if m.Role == "tool" && m.ToolCallID != "" {
			persisted[m.ToolCallID] = m.Content
		}
	}
	if got := persisted["c1"]; !strings.Contains(got, "fast-result") {
		t.Fatalf("completed call c1 result = %q, want its real output preserved", got)
	}
	if got := persisted["c2"]; got != "The operation was cancelled by the user." {
		t.Fatalf("interrupted call c2 result = %q, want the cancelled placeholder", got)
	}

	mu.Lock()
	defer mu.Unlock()
	if !onResult["c1"] {
		t.Fatal("completed call c1 reported success=false, want true")
	}
	if onResult["c2"] {
		t.Fatal("interrupted call c2 reported success=true, want false")
	}
}
