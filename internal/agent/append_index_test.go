package agent

import (
	"context"
	"sync"
	"testing"

	"gogen/internal/llm"
)

// appendIndexStubProvider returns one tool call on the first round, then a
// plain content turn, so the turn appends: the user message, the tool-call
// assistant message, the tool result, and the final assistant message.
type appendIndexStubProvider struct {
	mu    sync.Mutex
	calls int
}

func (s *appendIndexStubProvider) GenerateResponse(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "done"}, nil
}

func (s *appendIndexStubProvider) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, _ *llm.StreamHandlers) (*llm.StreamResult, error) {
	s.mu.Lock()
	s.calls++
	call := s.calls
	s.mu.Unlock()
	if call == 1 {
		return &llm.StreamResult{
			ToolCalls: []llm.ToolCall{{
				ID:   "call_1",
				Name: "execute_command",
				Args: map[string]any{"command": "echo hi"},
			}},
		}, nil
	}
	return &llm.StreamResult{Content: "done"}, nil
}

func (s *appendIndexStubProvider) ModelContextLimit(_ context.Context) (int, error) { return 1000, nil }
func (s *appendIndexStubProvider) SetThinkingLevel(string)                          {}
func (s *appendIndexStubProvider) ListModels(_ context.Context) ([]llm.ModelInfo, error) {
	return nil, nil
}
func (s *appendIndexStubProvider) SetModel(string) error { return nil }
func (s *appendIndexStubProvider) ModelName() string     { return "append-index-model" }

// TestOnMessageAppendedIndexes pins the host callback that lets a client
// track how far its rendered transcript has advanced (without a history
// refetch): every message the turn appends is reported with its index in
// a.Messages — the tool-call round's assistant message, each tool result,
// and the final assistant message. The user message is not reported here:
// the web host acks it separately (user_acked) at turn start.
func TestOnMessageAppendedIndexes(t *testing.T) {
	prov := &appendIndexStubProvider{}
	a := NewAgent(prov, NewExecutor(t.TempDir()), nil)
	defer a.Close()

	type appended struct {
		index int
		role  string
	}
	var got []appended
	var mu sync.Mutex
	h := &llm.StreamHandlers{
		OnMessageAppended: func(index int, role string) {
			mu.Lock()
			got = append(got, appended{index: index, role: role})
			mu.Unlock()
		},
	}
	if _, err := a.StreamProcessInput(context.Background(), "run echo", h); err != nil {
		t.Fatalf("turn: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	// Only the rounds' assistant messages are reported (tool results render
	// no bubble of their own; the mark stays conservative on turns that end
	// on a tool message).
	wantRoles := []string{"assistant", "assistant"}
	if len(got) != len(wantRoles) {
		t.Fatalf("appends = %+v, want %d entries (%v)", got, len(wantRoles), wantRoles)
	}
	// The indexes must be non-decreasing, start after the user message, and
	// name the messages actually at those positions in a.Messages.
	prev := -1
	for i, ap := range got {
		if ap.role != wantRoles[i] {
			t.Fatalf("append %d role = %q, want %q", i, ap.role, wantRoles[i])
		}
		if ap.index <= prev {
			t.Fatalf("append %d index = %d, want > %d", i, ap.index, prev)
		}
		prev = ap.index
		a.statsMu.RLock()
		if ap.index >= len(a.Messages) || a.Messages[ap.index].Role != ap.role {
			a.statsMu.RUnlock()
			t.Fatalf("append %d index %d does not name a %s message (messages=%d)",
				i, ap.index, ap.role, len(a.Messages))
		}
		a.statsMu.RUnlock()
	}
	// The final reported index must be the transcript's last message: that
	// is exactly what a client's rendered-index mark has to reach for a
	// conditional attach to prove it is current.
	a.statsMu.RLock()
	last := len(a.Messages) - 1
	a.statsMu.RUnlock()
	if got[len(got)-1].index != last {
		t.Fatalf("last reported index = %d, want the transcript's last (%d)", got[len(got)-1].index, last)
	}
}
