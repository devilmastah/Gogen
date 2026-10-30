package subagents

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestRegistryRegisterRefusesDuplicate pins that a second record for one id is
// refused instead of overwriting the first. The registered record owns the host
// runtime, its armed reply capture and its retention timer, so replacing it
// would strand all three — and the displaced record's release would evict the
// runtime the newcomer is using.
func TestRegistryRegisterRefusesDuplicate(t *testing.T) {
	r := newRegistry[*stubHandle]()
	first := &Child[*stubHandle]{ID: "c1", ParentID: "parent"}
	if !r.register(first) {
		t.Fatal("the first register was refused")
	}
	if r.register(&Child[*stubHandle]{ID: "c1", ParentID: "parent"}) {
		t.Fatal("a duplicate id was registered over the existing record")
	}
	if got := r.get("c1"); got != first {
		t.Fatalf("get = %p, want the first record %p", got, first)
	}
}

// TestRegistryBeginReviveExclusive pins the claim that makes the revive path
// atomic: one revival of an id at a time, never for an id that is already live,
// and released on the way out.
func TestRegistryBeginReviveExclusive(t *testing.T) {
	r := newRegistry[*stubHandle]()
	if !r.beginRevive("saved") {
		t.Fatal("the first revival was refused")
	}
	if r.beginRevive("saved") {
		t.Fatal("a second revival of the same id was admitted")
	}
	r.endRevive("saved")
	if !r.beginRevive("saved") {
		t.Fatal("the claim was not released by endRevive")
	}
	r.endRevive("saved")

	if !r.register(&Child[*stubHandle]{ID: "live", ParentID: "parent"}) {
		t.Fatal("register was refused")
	}
	if r.beginRevive("live") {
		t.Fatal("a live child was claimed for revival")
	}
}

// TestSendMessageRevivesOnceUnderConcurrency pins the check-then-act the claim
// closes: concurrent send_message calls for one SAVED child must reopen it once
// and share the reopened child, instead of building a second record (and a
// second host runtime) for the same session id.
//
// The tool can never run twice at once today — send_message is not
// parallel-eligible, and a session runs one turn at a time — so this is the
// registry primitive's guarantee rather than a reachable tool path.
func TestSendMessageRevivesOnceUnderConcurrency(t *testing.T) {
	host := newStubHost()
	host.policy = Policy{Retain: time.Hour}
	host.revivableParent = "parent"
	host.revivable = map[string]string{"saved-child": "subagent: revived"}
	// Widen the reopen, so every caller is inside it when they collide.
	host.reviveDelay = 20 * time.Millisecond
	sp := New[*stubHandle](host)
	parent := testAgent("parent", "m")
	parent.SetSessionID("parent")
	if sp.Get("saved-child") != nil {
		t.Fatal("precondition: the child must not be live")
	}

	const callers = 8
	errs := make([]error, callers)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i := 0; i < callers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			errs[i] = sp.SendMessage(parent, "saved-child", fmt.Sprintf("message %d", i))
		}(i)
	}
	close(start)
	wg.Wait()

	host.mu.Lock()
	revived := append([]string(nil), host.revived...)
	host.mu.Unlock()
	if len(revived) != 1 {
		t.Fatalf("revives = %v, want exactly one: concurrent callers must share one reopened child", revived)
	}
	if sp.Get("saved-child") == nil {
		t.Fatal("the revived child must be registered")
	}
	for i, err := range errs {
		if err == nil {
			continue // reached the child after it was reopened: a real delivery
		}
		if !strings.Contains(err.Error(), "being reopened") {
			t.Fatalf("caller %d = %v, want a delivery or the retry-able reclaim refusal", i, err)
		}
	}
}
