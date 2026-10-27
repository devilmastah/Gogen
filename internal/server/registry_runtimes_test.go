package server

import "testing"

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
