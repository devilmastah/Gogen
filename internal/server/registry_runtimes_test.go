package server

import (
	"testing"

	"gogen/internal/agent"
)

// TestRuntimesSnapshotMatchesRegisteredOrder pins the D5 helper: one lock, a
// snapshot in registration order, and no entry for an id whose runtime is gone
// (the old activeIDs()+get(id) fan-out took the lock N+1 times and looked each
// id back up).
func TestRuntimesSnapshotMatchesRegisteredOrder(t *testing.T) {
	r := newSessionRegistry(8)
	a, b, c := newTestRuntime(t), newTestRuntime(t), newTestRuntime(t)
	r.register("a", a)
	r.register("b", b)
	r.register("c", c)

	got := r.runtimes()
	if len(got) != 3 {
		t.Fatalf("runtimes = %d, want 3", len(got))
	}
	for i, want := range []*sessionRuntime{a, b, c} {
		if got[i] != want {
			t.Fatalf("runtimes[%d] = %p, want %p (registration order)", i, got[i], want)
		}
	}

	// Removing one drops it from the snapshot and skips the stale id.
	r.remove("b")
	got = r.runtimes()
	if len(got) != 2 || got[0] != a || got[1] != c {
		t.Fatalf("runtimes after remove = %v, want [a c]", got)
	}
	// The snapshot is a copy: mutating it must not affect the registry.
	got[0] = nil
	if r.runtimes()[0] != a {
		t.Fatal("runtimes returned the registry's own slice")
	}
}

// TestRegisteredSetCoversUnattendedRuntimes pins the payload flag the nested
// sidebar needs: `registered` reports every live runtime, including one nobody
// is viewing and no turn is running on — which `active` (viewer or running
// turn) deliberately excludes. Without it a background child between turns was
// indistinguishable in the payload from a child whose process is gone, so the
// row labelled a live, continuable child "interrupted" and flipped to "idle"
// as soon as somebody opened its pane.
func TestRegisteredSetCoversUnattendedRuntimes(t *testing.T) {
	r := newSessionRegistry(8)
	headless := newTestRuntime(t) // registered, no viewer, no turn (a held child)
	attended := newTestRuntime(t) // registered AND viewed
	r.register("headless", headless)
	r.register("attended", attended)
	attached := newFakeWSConn()
	attended.attach(attached)

	registered := r.registeredSet()
	for _, id := range []string{"headless", "attended"} {
		if !registered[id] {
			t.Fatalf("registeredSet missing %q: %v", id, registered)
		}
	}
	if active := r.activeSet(); active["headless"] || !active["attended"] {
		t.Fatalf("activeSet = %v, want only the viewed runtime", active)
	}

	// The wire entry carries it, so a client can tell the two apart.
	entries := sessionEntries(
		[]agent.SessionInfo{
			{ID: "headless", ParentID: "parent", SubagentStatus: "running"},
			{ID: "attended"},
			{ID: "gone", ParentID: "parent", SubagentStatus: "running"},
		},
		r.activeSet(), r.turnActiveSet(), registered)
	if len(entries) != 3 {
		t.Fatalf("sessionEntries = %d entries, want 3", len(entries))
	}
	if !entries[0].Registered || entries[0].Active {
		t.Fatalf("headless child: registered=%v active=%v, want registered and not active",
			entries[0].Registered, entries[0].Active)
	}
	if !entries[1].Registered || !entries[1].Active {
		t.Fatalf("viewed runtime: registered=%v active=%v, want both",
			entries[1].Registered, entries[1].Active)
	}
	if entries[2].Registered {
		t.Fatal("a session with no runtime must not claim to be registered")
	}
}
