package server

import (
	"context"
	"testing"

	"gogen/internal/agent"
	"gogen/internal/contextmgr"
	llmtest "gogen/internal/llm/llmtest"
)

// addTestClient marks rt as having one attached client. The routing decision
// only reads clientCount, so a placeholder socket is enough (no write happens
// on this path).
func addTestClient(rt *sessionRuntime) {
	rt.clientsMu.Lock()
	rt.clients[&wsConn{}] = struct{}{}
	rt.clientsMu.Unlock()
}

func testRuntime(t *testing.T, id string) *sessionRuntime {
	t.Helper()
	prov := llmtest.NewMockProvider()
	a := agent.NewAgent(prov, agent.NewExecutor(t.TempDir()), contextmgr.NewManager(prov, contextmgr.DefaultSettings()))
	a.SetSessionID(id)
	return newSessionRuntime(a)
}

// TestApprovalTargetRouting pins the ONE routing rule the delete and command
// forwarding approvers share: the child's own clients first, the live parent
// second, nobody → nil (deny fail-closed). Both approval kinds call it, so a
// drift here would strand one kind's prompt exactly as it did for commands
// before nested forwarding existed.
func TestApprovalTargetRouting(t *testing.T) {
	reg := newSessionRegistry(0)
	child := testRuntime(t, "child")
	parent := testRuntime(t, "parent")
	reg.register(child.agent.SessionID, child)
	reg.register(parent.agent.SessionID, parent)

	// Nobody attached anywhere: nobody can answer.
	if got := reg.approvalTarget(child, parent.agent.SessionID); got != nil {
		t.Fatalf("target = %v, want nil (unattended)", got)
	}

	// The parent has a viewer: the child's prompt goes there.
	addTestClient(parent)
	if got := reg.approvalTarget(child, parent.agent.SessionID); got != parent {
		t.Fatalf("target = %v, want the live parent", got)
	}

	// The child's own pane is open: it answers its own prompt.
	addTestClient(child)
	if got := reg.approvalTarget(child, parent.agent.SessionID); got != child {
		t.Fatalf("target = %v, want the child itself", got)
	}

	// The child's pane closes and the parent id is no longer live (evicted,
	// then reopened under the same id): nobody can answer, so the request must
	// deny instead of parking forever.
	child.clientsMu.Lock()
	child.clients = make(map[*wsConn]struct{})
	child.clientsMu.Unlock()
	reg.mu.Lock()
	delete(reg.agents, parent.agent.SessionID)
	reg.mu.Unlock()
	if got := reg.approvalTarget(child, parent.agent.SessionID); got != nil {
		t.Fatalf("target = %v, want nil once neither runtime has clients", got)
	}
}

// TestResolvedApproverPrefersOverride pins the override precedence shared by
// both approval kinds: an installed override (subagent D6 forwarding, board
// start deny-when-unattended) wins over the runtime's own approver, and a nil
// override leaves the default in place.
func TestResolvedApproverPrefersOverride(t *testing.T) {
	var defCalls, overrideCalls int
	def := func(context.Context, agent.CommandRequest) (bool, error) {
		defCalls++
		return true, nil
	}
	override := func(context.Context, agent.CommandRequest) (bool, error) {
		overrideCalls++
		return false, nil
	}

	if _, err := resolvedApprover(def, nil)(context.Background(), agent.CommandRequest{}); err != nil {
		t.Fatalf("default approver: %v", err)
	}
	if defCalls != 1 || overrideCalls != 0 {
		t.Fatalf("nil override must use the default (def=%d override=%d)", defCalls, overrideCalls)
	}

	if _, err := resolvedApprover(def, override)(context.Background(), agent.CommandRequest{}); err != nil {
		t.Fatalf("override approver: %v", err)
	}
	if overrideCalls != 1 || defCalls != 1 {
		t.Fatalf("installed override must win (def=%d override=%d)", defCalls, overrideCalls)
	}
}
