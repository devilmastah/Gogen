package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"gogen/internal/llm"
	"gogen/internal/session"
)

func TestTodoManagerSessionIsolation(t *testing.T) {
	dir := t.TempDir()
	store := &stubSessionStore{}
	a := &Agent{
		Provider:     &statsStubProvider{},
		WorkingDir:   dir,
		SessionStore: store,
		SessionID:    "session-a",
		TodoManager:  NewTodoManager(dir),
	}
	if _, err := a.TodoManager.AddTodo("from session a"); err != nil {
		t.Fatal(err)
	}
	a.persistTodos()

	snapA := store.sessions["session-a"]
	if snapA.Todos == nil || len(snapA.Todos.Items) != 1 {
		t.Fatalf("session-a todos = %#v", snapA.Todos)
	}

	_, handled, err := a.HandleSessionCommand(context.Background(), "/new", "session-b")
	if err != nil || !handled {
		t.Fatalf("handled=%v err=%v", handled, err)
	}
	if !a.TodoManager.Empty() {
		t.Fatalf("expected empty todos after /new, got %s", a.TodoManager.ListTodos())
	}
	if _, err := a.TodoManager.AddTodo("from session b"); err != nil {
		t.Fatal(err)
	}
	a.persistTodos()

	_, handled, err = a.HandleSessionCommand(context.Background(), "resume session-a", "")
	if err != nil || !handled {
		t.Fatalf("resume handled=%v err=%v", handled, err)
	}
	list := a.TodoManager.ListTodos()
	if !strings.Contains(list, "from session a") {
		t.Fatalf("missing session-a todo: %q", list)
	}
	if strings.Contains(list, "from session b") {
		t.Fatalf("session-b todo leaked: %q", list)
	}
}

func TestRestoreSessionReplacesTodos(t *testing.T) {
	a := &Agent{
		WorkingDir:  "/tmp/project",
		TodoManager: NewTodoManager("/tmp/project"),
	}
	_, _ = a.TodoManager.AddTodo("stale")
	a.RestoreSession(SessionSnapshot{
		WorkingDir: "/tmp/project",
		Todos: &TodoList{
			Items:  []TodoItem{{ID: 2, Text: "restored", Status: "pending"}},
			NextID: 3,
		},
		Messages: []llm.Message{{Role: "user", Content: "hi"}},
	}, a.SessionID)
	got := a.TodoManager.ListTodos()
	if !strings.Contains(got, "restored") || strings.Contains(got, "stale") {
		t.Fatalf("todos=%q", got)
	}
}

func TestRestoreSessionClearsTodosWhenMissing(t *testing.T) {
	a := &Agent{
		WorkingDir:  "/tmp/project",
		TodoManager: NewTodoManager("/tmp/project"),
	}
	_, _ = a.TodoManager.AddTodo("leak")
	a.RestoreSession(SessionSnapshot{
		WorkingDir: "/tmp/project",
		Messages:   []llm.Message{{Role: "user", Content: "hi"}},
	}, a.SessionID)
	if !a.TodoManager.Empty() {
		t.Fatalf("expected empty todos, got %s", a.TodoManager.ListTodos())
	}
}

func TestImportLegacyFileOnce(t *testing.T) {
	dir := t.TempDir()
	legacy := filepath.Join(dir, ".gogen", "todos.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o755); err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(TodoList{
		Items:  []TodoItem{{ID: 1, Text: "legacy task", Status: "pending"}},
		NextID: 2,
	})
	if err := os.WriteFile(legacy, data, 0o644); err != nil {
		t.Fatal(err)
	}

	tm := NewTodoManager(dir)
	if !tm.ImportLegacyFile() {
		t.Fatal("expected legacy import")
	}
	if tm.Empty() || !strings.Contains(tm.ListTodos(), "legacy task") {
		t.Fatalf("todos=%q", tm.ListTodos())
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy file should be renamed away, err=%v", err)
	}
	if _, err := os.Stat(legacy + ".migrated"); err != nil {
		t.Fatalf("expected migrated backup: %v", err)
	}

	tm2 := NewTodoManager(dir)
	if tm2.ImportLegacyFile() {
		t.Fatal("legacy should not import again")
	}
}

func TestTodoManagerConcurrentAccess(t *testing.T) {
	// TodoManager must be safe to snapshot (flush paths hold no turn lock:
	// ShutdownSessions after a drain timeout, sessionDelete with a stuck
	// turn) while a turn mutates todos. Run this under -race to verify.
	m := NewTodoManager(t.TempDir())
	var wg sync.WaitGroup

	// Mutators: todo tools on the "turn" goroutine.
	for g := 0; g < 4; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				text := fmt.Sprintf("task %d-%d", g, i)
				_, _ = m.AddTodo(text)
				if i%10 == 0 {
					_ = m.Snapshot()
					_ = m.ListTodos()
				}
				if i%25 == 0 {
					_, _ = m.ClearDoneTodos()
					_, _ = m.DoneTodo(1)
				}
			}
		}(g)
	}

	// Readers: doPersist-style snapshots and legacy-file saves.
	for g := 0; g < 2; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_ = m.Snapshot()
				_ = m.Empty()
				_ = m.saveLegacy()
			}
		}()
	}
	wg.Wait()

	if m.Empty() {
		t.Fatalf("expected todos to survive concurrent access")
	}
}

// TestImportLegacyTodosPersistsFreshSession pins the migrated-todos
// persistence: the legacy import adopts .gogen/todos.json into a FRESH
// session (0 messages, no label) and renames the source file away. Skip's
// empty-session rule used to swallow the import's flush, so quitting before
// the first turn lost the imported list permanently. The session must now
// own the todos on disk right after the import.
func TestImportLegacyTodosPersistsFreshSession(t *testing.T) {
	dir := t.TempDir()
	store := session.NewStoreWithOptions(true, session.StoreOptions{})
	a := &Agent{
		Provider:     &statsStubProvider{},
		WorkingDir:   dir,
		SessionStore: store,
		SessionID:    "fresh-session",
		TodoManager:  NewTodoManager(dir),
	}

	legacy := session.TodoList{
		Items:  []session.TodoItem{{ID: 1, Text: "legacy task", Status: "pending"}},
		NextID: 2,
	}
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, ".gogen", "todos.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}

	if !a.ImportLegacyTodos() {
		t.Fatal("legacy todos were not imported")
	}
	// The quit sweep (FlushPending) cannot save a clean todos-only session —
	// the import's own flush must have already written it.
	a.FlushPending()

	loaded, err := store.LoadInWorkingDir(dir, "fresh-session")
	if err != nil {
		t.Fatalf("todos-only session must be persisted after the import: %v", err)
	}
	if loaded.Todos == nil || len(loaded.Todos.Items) != 1 || loaded.Todos.Items[0].Text != "legacy task" {
		t.Fatalf("persisted todos=%+v, want the imported item", loaded.Todos)
	}
}
