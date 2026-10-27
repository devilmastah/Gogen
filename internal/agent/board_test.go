package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
)

func newTestBoard(t *testing.T) *BoardManager {
	t.Helper()
	return NewBoardManager(t.TempDir(), false)
}

// TestBoardMissingIndexDoesNotOverwriteTickets pins the recovery path: with
// index.json gone but ticket files present, the next Add must allocate a fresh
// ID instead of restarting at 1 and clobbering the existing ticket's file (the
// board then lost a card and its column order).
func TestBoardMissingIndexDoesNotOverwriteTickets(t *testing.T) {
	dir := t.TempDir() + "/" + "board"
	m := NewBoardManager(dir, false)
	if _, err := m.Add("first", "", "low", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("second", "", "low", "user"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(m.indexPath()); err != nil {
		t.Fatal(err)
	}

	// A fresh manager (the next process) sees no index and must rebuild it.
	m2 := NewBoardManager(dir, false)
	out, err := m2.Add("third", "", "low", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "#3") {
		t.Fatalf("add output = %q, want #3", out)
	}
	// The two earlier tickets still exist with unchanged titles.
	list, err := m2.List()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"first", "second", "third"} {
		if !strings.Contains(list, want) {
			t.Fatalf("board list lost %q:\n%s", want, list)
		}
	}
}

// TestBoardLifecycle covers add → list → show → claim → move → comment →
// block → done and the persisted file layout (index.json + one file per
// ticket).
func TestBoardLifecycle(t *testing.T) {
	m := newTestBoard(t)

	out, err := m.Add("Fix parser crash", "make go test pass", "high", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "#1") {
		t.Fatalf("add output = %q, want #1", out)
	}
	// One ticket file + index.json.
	entries, _ := os.ReadDir(m.dir)
	if len(entries) != 2 {
		t.Fatalf("expected 2 files (index + ticket), got %d", len(entries))
	}

	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "Fix parser crash") || !strings.Contains(list, "1 pending") {
		t.Fatalf("list = %q", list)
	}

	show, err := m.Show("1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(show, "high") || !strings.Contains(show, "make go test pass") {
		t.Fatalf("show = %q", show)
	}

	if _, err := m.Claim("1", "agent-a"); err != nil {
		t.Fatal(err)
	}
	// A second claim by someone else must fail.
	if _, err := m.Claim("1", "agent-b"); err == nil {
		t.Fatal("second claim should fail")
	}
	// Claiming by the same assignee is idempotent.
	if _, err := m.Claim("1", "agent-a"); err != nil {
		t.Fatalf("re-claim by same assignee: %v", err)
	}

	if _, err := m.Move("1", "in_review", "agent-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Comment("1", "ready for review", "agent-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Block("1", "waiting on upstream fix", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Done("1", "user"); err != nil {
		t.Fatal(err)
	}

	show, err = m.Show("1")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Status: done", "claimed", "moved to in_review", "ready for review", "blocked: waiting on upstream fix", "marked done"} {
		if !strings.Contains(show, want) {
			t.Fatalf("show missing %q:\n%s", want, show)
		}
	}

	// Reload from disk: a fresh manager must see the same board.
	m2 := NewBoardManager(m.dir, false)
	m2.dir = m.dir // same dir (NewBoardManager would use the parent's .gogen)
	list2, err := m2.List()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list2, "1 done") {
		t.Fatalf("reloaded list = %q", list2)
	}
}

// TestBoardClaimAssignsInProgressCard verifies claiming a card that was
// moved to in_progress without an assignee (drag-drop / Move) records the
// assignment instead of silently no-op'ing — the agent must not believe it
// claimed a card that has no assignee.
func TestBoardClaimAssignsInProgressCard(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("pre-moved", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Move("1", "in_progress", "user"); err != nil {
		t.Fatal(err)
	}
	out, err := m.Claim("1", "agent-a")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Claimed board item #1") {
		t.Fatalf("claim output = %q", out)
	}
	show, err := m.Show("1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(show, "Assignee: agent-a") || !strings.Contains(show, "claimed") {
		t.Fatalf("claim not recorded on pre-moved card:\n%s", show)
	}
	// A second agent still cannot take an assigned card.
	if _, err := m.Claim("1", "agent-b"); err == nil {
		t.Fatal("second claim by another agent should fail")
	}
}

// TestBoardValidation covers unknown columns, missing items, and empty
// required fields.
func TestBoardValidation(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("   ", "", "", "user"); err == nil {
		t.Fatal("empty title should fail")
	}
	if _, err := m.Add("ok", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Move("1", "nonsense", "user"); err == nil {
		t.Fatal("unknown column should fail")
	}
	if _, err := m.Show("99"); err == nil {
		t.Fatal("missing item should fail")
	}
	if _, err := m.Block("1", "  ", "user"); err == nil {
		t.Fatal("empty block reason should fail")
	}
	if _, err := m.Comment("1", "  ", "user"); err == nil {
		t.Fatal("empty comment should fail")
	}
	if _, err := m.Done("99", "user"); err == nil {
		t.Fatal("done on missing item should fail")
	}
}

// TestBoardUpdate verifies the partial-edit contract: nil fields are left
// unchanged, a provided empty title is rejected, description/priority can be
// cleared, a no-op edit records nothing, and the change persists.
func TestBoardUpdate(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("Original", "desc", "high", "user"); err != nil {
		t.Fatal(err)
	}

	// Partial edit: only the title; description/priority are untouched.
	title := "Renamed"
	if _, err := m.Update("1", BoardUpdate{Title: &title}, "user"); err != nil {
		t.Fatal(err)
	}
	item, err := m.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Title != "Renamed" || item.Description != "desc" || item.Priority != "high" {
		t.Fatalf("partial edit = %q / %q / %q, want Renamed / desc / high", item.Title, item.Description, item.Priority)
	}
	show, _ := m.Show("1")
	if !strings.Contains(show, "edited title") {
		t.Fatalf("activity missing the edited-field entry:\n%s", show)
	}

	// A provided empty title is rejected.
	empty := "   "
	if _, err := m.Update("1", BoardUpdate{Title: &empty}, "user"); err == nil {
		t.Fatal("empty title should fail")
	}

	// Description and priority can be cleared with an explicit empty value.
	blank := ""
	if _, err := m.Update("1", BoardUpdate{Description: &blank, Priority: &blank}, "user"); err != nil {
		t.Fatal(err)
	}
	item, _ = m.Item("1")
	if item.Description != "" || item.Priority != "" {
		t.Fatalf("cleared fields = %q / %q, want empty", item.Description, item.Priority)
	}

	// Context is a fourth editable field.
	ctx := "run go test ./internal/parser"
	if _, err := m.Update("1", BoardUpdate{Context: &ctx}, "user"); err != nil {
		t.Fatal(err)
	}
	item, _ = m.Item("1")
	if item.Context != ctx {
		t.Fatalf("context = %q, want %q", item.Context, ctx)
	}
	show, _ = m.Show("1")
	if !strings.Contains(show, "Context:") || !strings.Contains(show, ctx) {
		t.Fatalf("show missing context:\n%s", show)
	}

	// A no-op edit records no activity entry.
	before := len(item.Activity)
	same := "Renamed"
	out, err := m.Update("1", BoardUpdate{Title: &same}, "user")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "unchanged") {
		t.Fatalf("no-op output = %q, want unchanged", out)
	}
	item, _ = m.Item("1")
	if len(item.Activity) != before {
		t.Fatalf("no-op edit changed activity: %d -> %d", before, len(item.Activity))
	}

	// Unknown ticket.
	if _, err := m.Update("99", BoardUpdate{Title: &same}, "user"); err == nil {
		t.Fatal("update on unknown ticket should fail")
	}

	// Persistence: a reloaded manager sees the edit and its activity entry.
	m2 := NewBoardManager(t.TempDir(), false)
	m2.dir = m.dir
	item2, err := m2.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item2.Title != "Renamed" {
		t.Fatalf("reloaded title = %q, want Renamed", item2.Title)
	}
}

// TestBoardUpdateTool pins the board tool's update action: only the fields
// present in the call change (so an agent can rename without wiping the
// description), and an empty title is rejected.
func TestBoardUpdateTool(t *testing.T) {
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	a := NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
	a.SetBoardEnabled(true)
	bm := NewBoardManager(t.TempDir(), false)
	a.SetBoardManager(bm)
	if _, err := bm.Add("original", "keep me", "low", "user"); err != nil {
		t.Fatal(err)
	}

	out, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{
		"action": "update", "id": 1, "title": "renamed",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Updated board item #1") {
		t.Fatalf("update output = %q", out)
	}
	item, _ := bm.Item("1")
	if item.Title != "renamed" || item.Description != "keep me" || item.Priority != "low" {
		t.Fatalf("tool update = %q / %q / %q, want renamed / keep me / low", item.Title, item.Description, item.Priority)
	}

	// Context is editable on its own (a partial edit must not clobber the
	// other fields).
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{
		"action": "update", "id": "1", "context": "deploy notes in docs/deploy.md",
	}}); err != nil {
		t.Fatal(err)
	}
	item, _ = bm.Item("1")
	if item.Context != "deploy notes in docs/deploy.md" {
		t.Fatalf("context = %q", item.Context)
	}
	if item.Title != "renamed" || item.Description != "keep me" || item.Priority != "low" {
		t.Fatalf("context-only update clobbered other fields: %+v", item)
	}

	// An empty title is rejected.
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{
		"action": "update", "id": "1", "title": "  ",
	}}); err == nil {
		t.Fatal("empty title should fail")
	}
}

// TestBoardClaimConcurrency verifies concurrent claims on the same ticket
// from different agents serialize: exactly one assignee wins (run under
// -race).
func TestBoardClaimConcurrency(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("contested", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, errs[i] = m.Claim("1", fmt.Sprintf("agent-%d", i))
		}(i)
	}
	wg.Wait()
	successes := 0
	for _, err := range errs {
		if err == nil {
			successes++
		}
	}
	if successes != 1 {
		t.Fatalf("expected exactly 1 successful claim, got %d", successes)
	}
	show, err := m.Show("1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(show, "in_progress") {
		t.Fatalf("ticket should be in_progress:\n%s", show)
	}
}

// TestBoardItemCap verifies the 200-ticket cap (D4).
func TestBoardItemCap(t *testing.T) {
	m := newTestBoard(t)
	for i := 0; i < 200; i++ {
		if _, err := m.Add(fmt.Sprintf("item %d", i), "", "", "user"); err != nil {
			t.Fatalf("add %d: %v", i, err)
		}
	}
	if _, err := m.Add("overflow", "", "", "user"); err == nil {
		t.Fatal("201st item should fail")
	}
}

// TestBoardActivityCap verifies the activity log caps at 50 entries per
// ticket (D4).
func TestBoardActivityCap(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("chatty", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 60; i++ {
		if _, err := m.Comment("1", fmt.Sprintf("note %d", i), "user"); err != nil {
			t.Fatal(err)
		}
	}
	data, err := os.ReadFile(m.itemPath("1"))
	if err != nil {
		t.Fatal(err)
	}
	var item BoardItem
	if err := json.Unmarshal(data, &item); err != nil {
		t.Fatal(err)
	}
	if len(item.Activity) != 50 {
		t.Fatalf("activity entries = %d, want 50", len(item.Activity))
	}
	if item.Activity[0].Text != "note 10" {
		t.Fatalf("oldest kept entry = %q, want note 10 (oldest dropped)", item.Activity[0].Text)
	}
}

// TestBoardCorruptIndexRebuild verifies a corrupt index.json is rebuilt from
// the ticket files (status is authoritative on the tickets).
func TestBoardCorruptIndexRebuild(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("a", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("b", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Move("2", "in_progress", "agent"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.indexPath(), []byte("{corrupt"), 0o644); err != nil {
		t.Fatal(err)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(list, "a") || !strings.Contains(list, "b") {
		t.Fatalf("rebuilt list = %q", list)
	}
	show, err := m.Show("2")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(show, "in_progress") {
		t.Fatalf("status lost in rebuild:\n%s", show)
	}
	// Next add continues after the max existing id.
	out, err := m.Add("c", "", "", "user")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "#3") {
		t.Fatalf("add after rebuild = %q, want #3", out)
	}
}

// TestAtoiIDParsing pins the id-parsing behavior the index rebuild path
// relies on: an empty id must error (it would otherwise parse as 0 with no
// error), so the rebuild skips empty-named files instead of adding a bogus
// "" entry to the column order.
func TestAtoiIDParsing(t *testing.T) {
	if _, err := strconv.Atoi(""); err == nil {
		t.Fatal("empty id should error")
	}
	if _, err := strconv.Atoi("12"); err != nil {
		t.Fatalf("numeric id should parse: %v", err)
	}
	if _, err := strconv.Atoi("12a"); err == nil {
		t.Fatal("non-numeric id should error")
	}
}

// TestBoardGlobalDir verifies the global-mode board root (D3).
func TestBoardGlobalDir(t *testing.T) {
	m := NewBoardManager("/some/project", true)
	if m.dir == filepath.Join("/some/project", ".gogen", "board") {
		t.Fatal("global mode must not use the project board dir")
	}
	if !strings.Contains(m.dir, "board") {
		t.Fatalf("global board dir = %q", m.dir)
	}
	m2 := NewBoardManager("/other/project", false)
	if m2.dir != filepath.Join("/other/project", ".gogen", "board") {
		t.Fatalf("project board dir = %q", m2.dir)
	}
}

// TestBoardChangedHook verifies the on-board-changed hook fires after every
// successful board mutation through the agent tool with the mutation's
// output message (the web server broadcasts board_state + a success toast
// from it) and does NOT fire for read-only actions or failed mutations.
func TestBoardChangedHook(t *testing.T) {
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	a := NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
	a.SetBoardEnabled(true)
	bm := NewBoardManager(t.TempDir(), false)
	a.SetBoardManager(bm)

	var fired []string
	a.SetOnBoardChanged(func(msg string) { fired = append(fired, msg) })

	// Read-only: no fire.
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{"action": "list"}}); err != nil {
		t.Fatal(err)
	}
	// Failed mutation: no fire.
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{"action": "move", "id": "99", "column": "done"}}); err == nil {
		t.Fatal("expected error for missing item")
	}
	if len(fired) != 0 {
		t.Fatalf("hook fired %d times for read/failed calls, want 0", len(fired))
	}

	// Mutations: each fires exactly once with its output message.
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{"action": "add", "title": "hook test"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{"action": "claim", "id": "1"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{"action": "done", "id": "1"}}); err != nil {
		t.Fatal(err)
	}
	if len(fired) != 3 {
		t.Fatalf("hook fired %d times, want 3", len(fired))
	}
	for i, want := range []string{"Added board item #1: hook test", "Claimed board item #1: hook test", "Board item #1 is done: hook test"} {
		if fired[i] != want {
			t.Fatalf("hook message %d = %q, want %q", i, fired[i], want)
		}
	}
}

// TestBoardDelete verifies the remove action: the ticket file and index
// entry are gone, the list no longer shows it, and deleting a missing card
// errors.
func TestBoardDelete(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("doomed", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add("keeper", "", "", "user"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Move("1", "in_progress", "agent"); err != nil {
		t.Fatal(err)
	}
	out, err := m.Delete("1")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Removed board item #1") {
		t.Fatalf("delete output = %q", out)
	}
	// File gone, index clean, list shows only the survivor.
	if _, err := os.Stat(m.itemPath("1")); !os.IsNotExist(err) {
		t.Fatalf("ticket file still exists (err=%v)", err)
	}
	list, err := m.List()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list, "doomed") || !strings.Contains(list, "keeper") {
		t.Fatalf("list after delete = %q", list)
	}
	if _, err := m.Delete("1"); err == nil {
		t.Fatal("deleting a missing card should fail")
	}
	// A fresh manager (reload from disk) agrees.
	m2 := NewBoardManager(t.TempDir(), false)
	m2.dir = m.dir
	list2, err := m2.List()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(list2, "doomed") {
		t.Fatalf("reloaded list still shows deleted card: %q", list2)
	}
}

// TestBoardToolGating verifies the MCP-style zero-trace gating: with the
// feature off the board tool is absent from llmTools/AllowedToolNames and
// executeTool returns "unknown tool"; with it on (and a manager attached) it
// is exposed and runs; in plan mode it stays available (D7).
func TestBoardToolGating(t *testing.T) {
	prov := llmtest.NewMockProvider()
	exec := NewExecutor(t.TempDir())
	a := NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))

	// Off: no trace anywhere.
	for _, def := range a.llmTools() {
		if def.Name == "board" {
			t.Fatal("board tool must not be exposed when disabled")
		}
	}
	if _, ok := a.AllowedToolNames()["board"]; ok {
		t.Fatal("board must not be allowed when disabled")
	}
	if _, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{"action": "list"}}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("expected unknown tool error, got %v", err)
	}

	// On: exposed, callable, and available in plan mode.
	a.SetBoardEnabled(true)
	a.SetBoardManager(NewBoardManager(t.TempDir(), false))
	found := false
	for _, def := range a.llmTools() {
		if def.Name == "board" {
			found = true
		}
	}
	if !found {
		t.Fatal("board tool should be exposed when enabled")
	}
	if _, ok := a.AllowedToolNames()["board"]; !ok {
		t.Fatal("board should be allowed when enabled")
	}
	out, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{"action": "list"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Board is empty") {
		t.Fatalf("list output = %q", out)
	}
	a.SetMode(ModePlan)
	if _, ok := a.AllowedToolNames()["board"]; !ok {
		t.Fatal("board should stay allowed in plan mode (D7)")
	}
	if err := a.checkPlanMode("board"); err != nil {
		t.Fatalf("plan mode should allow board (D7): %v", err)
	}
	out, err = a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{"action": "add", "title": "plan-mode card"}})
	if err != nil {
		t.Fatalf("board mutation in plan mode should work (D7): %v", err)
	}
	if !strings.Contains(out, "#1") {
		t.Fatalf("add output = %q", out)
	}
}

// TestSetStartOptions verifies the per-ticket start-option persistence
// (model + prompt template + reasoning-effort level chosen in the web
// "Start agent" popover): stored on the ticket, surviving a reload from
// disk, and cleared by an explicit empty write. The start op is
// authoritative — an empty value resets the override.
func TestSetStartOptions(t *testing.T) {
	m := newTestBoard(t)
	if _, err := m.Add("Fix parser crash", "make go test pass", "high", "user"); err != nil {
		t.Fatal(err)
	}

	// Unknown ticket.
	if err := m.SetStartOptions("99", "gpt-4o-mini", "custom", "high"); err == nil {
		t.Fatal("SetStartOptions on an unknown ticket should fail")
	}

	// Persist all options.
	if err := m.SetStartOptions("1", "gpt-4o-mini", "custom {title}", "high"); err != nil {
		t.Fatal(err)
	}
	item, err := m.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Model != "gpt-4o-mini" || item.Prompt != "custom {title}" || item.ThinkingLevel != "high" {
		t.Fatalf("stored start options = %q / %q / %q, want gpt-4o-mini / custom {title} / high", item.Model, item.Prompt, item.ThinkingLevel)
	}

	// The level is canonicalized on persist: a client sending "HIGH" must
	// store "high" (the value the start handler applies and the popover's
	// lowercase chips compare against).
	if err := m.SetStartOptions("1", "gpt-4o-mini", "custom {title}", "  HIGH "); err != nil {
		t.Fatal(err)
	}
	item, err = m.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item.ThinkingLevel != "high" {
		t.Fatalf("normalized start level = %q, want high", item.ThinkingLevel)
	}

	// Reload from disk: a fresh manager must see the same options.
	m2 := NewBoardManager(m.dir, false)
	m2.dir = m.dir // same dir (NewBoardManager would use the parent's .gogen)
	item2, err := m2.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item2.Model != "gpt-4o-mini" || item2.Prompt != "custom {title}" || item2.ThinkingLevel != "high" {
		t.Fatalf("reloaded start options = %q / %q / %q", item2.Model, item2.Prompt, item2.ThinkingLevel)
	}

	// An explicit empty write clears back to the defaults.
	if err := m.SetStartOptions("1", "", "", ""); err != nil {
		t.Fatal(err)
	}
	item, err = m.Item("1")
	if err != nil {
		t.Fatal(err)
	}
	if item.Model != "" || item.Prompt != "" || item.ThinkingLevel != "" {
		t.Fatalf("cleared start options = %q / %q / %q, want empty", item.Model, item.Prompt, item.ThinkingLevel)
	}
}

// TestBoardIDSurvivesToolCall pins the end-to-end board tool behavior for
// the id-taking actions: board ids are numeric strings, so the handler must
// accept unquoted JSON numbers (float64 as decoded from model JSON), ints,
// and quoted numeric strings — all targeting the same card. Failure modes:
// a non-numeric string surfaces the real type error (not "missing"), zero
// and absent ids are rejected.
func TestBoardIDSurvivesToolCall(t *testing.T) {
	newAgent := func() (*Agent, *BoardManager) {
		prov := llmtest.NewMockProvider()
		exec := NewExecutor(t.TempDir())
		a := NewAgent(prov, exec, contextmgr.NewManager(prov, contextmgr.Settings{ContextLimit: 128000}))
		a.SetBoardEnabled(true)
		bm := NewBoardManager(t.TempDir(), false)
		a.SetBoardManager(bm)
		if _, err := bm.Add("first card", "", "", "user"); err != nil {
			t.Fatal(err)
		}
		return a, bm
	}

	t.Run("float64 id (JSON number) marks done", func(t *testing.T) {
		a, _ := newAgent()
		out, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{
			"action": "done",
			"id":     float64(1), // native tool-call JSON decode shape
		}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "#1") {
			t.Fatalf("expected #1 in output, got %q", out)
		}
		item, err := a.BoardManager().Item("1")
		if err != nil || item.Status != "done" {
			t.Fatalf("item #1 status = %q (err=%v), want done", item.Status, err)
		}
	})

	t.Run("int id moves", func(t *testing.T) {
		a, _ := newAgent()
		out, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{
			"action": "move",
			"id":     1,
			"column": "in_review",
		}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "Moved board item #1 to in_review") {
			t.Fatalf("move output = %q", out)
		}
	})

	t.Run("quoted numeric string id claims", func(t *testing.T) {
		a, _ := newAgent()
		out, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{
			"action": "claim",
			"id":     "1",
		}})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !strings.Contains(out, "Claimed board item #1") {
			t.Fatalf("claim output = %q", out)
		}
	})

	t.Run("non-numeric string id reports type error not missing", func(t *testing.T) {
		a, _ := newAgent()
		_, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{
			"action": "done",
			"id":     "abc",
		}})
		if err == nil {
			t.Fatal("expected error for non-numeric string id")
		}
		if !strings.Contains(err.Error(), "must be an integer") {
			t.Fatalf("expected type error, got: %v", err)
		}
		if strings.Contains(err.Error(), "missing required argument") {
			t.Fatalf("non-numeric id must not be masked as missing: %v", err)
		}
	})

	t.Run("zero id rejected", func(t *testing.T) {
		a, _ := newAgent()
		_, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{
			"action": "show",
			"id":     0,
		}})
		if err == nil || !strings.Contains(err.Error(), "must be a positive integer") {
			t.Fatalf("got %v, want positive-integer error", err)
		}
	})

	t.Run("absent id reports missing", func(t *testing.T) {
		a, _ := newAgent()
		_, err := a.executeTool(t.Context(), llm.ToolCall{Name: "board", Args: map[string]any{
			"action": "done",
		}})
		if err == nil {
			t.Fatal("expected error for absent id")
		}
		if !strings.Contains(err.Error(), `missing required argument "id"`) {
			t.Fatalf("expected missing-argument error, got: %v", err)
		}
	})
}
