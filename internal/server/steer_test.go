package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/llm"
	"gogen/internal/session"
	"gogen/internal/steerq"
)

// userMsgIndex returns the index of the first user message containing
// needle (-1 when absent) — the FIFO-order probe for steering delivery.
func userMsgIndex(a *agent.Agent, needle string) int {
	return userMsgIndexIn(a.SnapshotMessages(), needle)
}

// userMsgIndexIn is userMsgIndex over one snapshot, so several indexes and
// hasAssistantBetween can read the same transcript.
func userMsgIndexIn(msgs []llm.Message, needle string) int {
	for i, m := range msgs {
		if m.Role == "user" && strings.Contains(m.Content, needle) {
			return i
		}
	}
	return -1
}

// waitTurnsPersisted waits until minDone released blocking-stub turns' final
// state is ON DISK (each completed turn ends with the assistant "done"
// reply) — a user message persists at turn START, so only the assistant
// replies prove the turns' closing flushes landed; ending the test earlier
// races TempDir teardown (the suite's documented "directory not empty"
// flake). Every completed turn in the test must be released and counted.
func waitTurnsPersisted(t *testing.T, store *session.Store, dir, sessionID string, minDone int) {
	t.Helper()
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(dir, sessionID)
		if err != nil {
			return false
		}
		done := 0
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "done" {
				done++
			}
		}
		return done >= minDone
	})
}

// startBlockedTurn acquires the runtime's turn lock and hands it to
// startTurn (whose goroutine defers the unlock), waiting until the turn is
// actually inside the provider — the same pattern deliver_test.go uses so
// the queue cannot be drained mid-arrangement.
func startBlockedTurn(t *testing.T, rt *sessionRuntime, stub *blockingStub, content string) {
	t.Helper()
	rt.turnMu.Lock()
	rt.startTurn(nil, content, nil)
	stub.waitBlocked(1)
}

// TestUserSteerEnqueueDrainsFIFO pins the steering core: messages typed
// while a turn runs are queued and drain in strict FIFO arrival order —
// user items and system deliveries share one queue — one item per turn,
// each as its own user turn.
func TestUserSteerEnqueueDrainsFIFO(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}

	startBlockedTurn(t, rt, stub, "block me")
	if !rt.enqueueUserTurn("q1", "steer-a", nil) {
		t.Fatal("first steer enqueue refused")
	}
	if !rt.enqueueUserTurn("q2", "steer-b", nil) {
		t.Fatal("second steer enqueue refused")
	}
	rt.deliverToSession("sys notice")

	// Nothing is delivered while the turn runs.
	time.Sleep(200 * time.Millisecond)
	if userMsgIndex(a, "steer-a") >= 0 || userMsgIndex(a, "steer-b") >= 0 {
		t.Fatal("steering messages must not be injected mid-turn")
	}

	// End the blocking turn: the worker wakes at turn end and drains the
	// queue one item per turn. Each delivered turn blocks the stub, so the
	// test releases calls as their user message lands.
	rt.stream.cancelInFlight()
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "steer-a") >= 0 })
	stub.releaseN(2)
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "steer-b") >= 0 })
	stub.releaseN(3)
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "sys notice") >= 0 })

	msgs := a.SnapshotMessages()
	ia, ib, is := userMsgIndexIn(msgs, "steer-a"), userMsgIndexIn(msgs, "steer-b"), userMsgIndexIn(msgs, "sys notice")
	if !(ia < ib && ib < is) {
		t.Fatalf("FIFO order violated: steer-a@%d, steer-b@%d, sys@%d", ia, ib, is)
	}
	// Each queued item ran as its OWN turn: an assistant reply between the
	// two user messages.
	if !hasAssistantBetween(msgs, ia, ib) {
		t.Fatalf("no assistant reply between steer-a@%d and steer-b@%d, want one turn each", ia, ib)
	}

	stub.releaseN(4)
	// The delivered turns' final flushes must land before TempDir teardown
	// (the store-wait pattern used across this suite): three completed
	// turns (steer-a, steer-b, sys notice).
	waitTurnsPersisted(t, store, dir, a.SessionID, 3)
}

// hasAssistantBetween reports whether an assistant message sits between the
// messages at indexes from and to. The indexes must come from msgs.
func hasAssistantBetween(msgs []llm.Message, from, to int) bool {
	for i := from + 1; i < to && i < len(msgs); i++ {
		if msgs[i].Role == "assistant" {
			return true
		}
	}
	return false
}

// TestUserSteerJoinQueued pins join_queued: user messages queued during one
// turn leave together as ONE user message, with every image in queue order,
// the queue_update that starts it carries every id and the delivered text,
// and the notice behind them stays its own turn.
func TestUserSteerJoinQueued(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	s.ws.SetJoinQueued(true)
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}
	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	startBlockedTurn(t, rt, stub, "block me")
	imgA := llm.ImageInput{DataURL: "data:image/png;base64,AAAA"}
	imgB := llm.ImageInput{DataURL: "data:image/png;base64,BBBB"}
	if !rt.enqueueUserTurn("q1", "steer-a", []llm.ImageInput{imgA}) || !rt.enqueueUserTurn("q2", "steer-b", []llm.ImageInput{imgB}) {
		t.Fatal("steer enqueue refused")
	}
	rt.deliverToSession("sys notice")

	rt.stream.cancelInFlight()
	started := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "queue_update" && len(m.QueueLeft) > 0
	})
	if strings.Join(started.QueueLeft, ",") != "q1,q2" || started.Content != "steer-a\n\nsteer-b" {
		t.Fatalf("start frame = left %v content %q, want both ids and the joined text", started.QueueLeft, started.Content)
	}
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "steer-a") >= 0 })
	ia := userMsgIndex(a, "steer-a")
	if ib := userMsgIndex(a, "steer-b"); ib != ia {
		t.Fatalf("steer-a@%d steer-b@%d, want one joined message", ia, ib)
	}
	images := a.SnapshotMessages()[ia].Images
	if len(images) != 2 || images[0].DataURL != imgA.DataURL || images[1].DataURL != imgB.DataURL {
		t.Fatalf("joined message images = %+v, want both in queue order", images)
	}
	stub.releaseN(2)
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "sys notice") >= 0 })
	msgs := a.SnapshotMessages()
	if ia, is := userMsgIndexIn(msgs, "steer-a"), userMsgIndexIn(msgs, "sys notice"); !hasAssistantBetween(msgs, ia, is) {
		t.Fatalf("sys notice @%d ran in the joined turn @%d, want its own turn", is, ia)
	}
	stub.releaseN(3)
	waitTurnsPersisted(t, store, dir, a.SessionID, 2)
}

// TestSendNowGating pins when Send now may stop the running turn: only while
// a model turn runs, the turn is not a send_message reply, and the named
// user item (or, unnamed, any user item) is still queued. Anything else must
// leave the turn alone.
func TestSendNowGating(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}
	expect := func(name, id string, want steerq.SendNowResult) {
		t.Helper()
		if got := rt.sendNow(id); got != want {
			t.Fatalf("%s: sendNow(%q) = %v, want %v", name, id, got, want)
		}
	}
	expect("no turn running", "", steerq.SendNowIdle)
	// A compaction marks the runtime busy without a model turn.
	rt.setTurnActive(true, time.Now(), nil)
	expect("busy without a model turn", "", steerq.SendNowNoModelCall)
	rt.setTurnActive(false, time.Time{}, nil)

	startBlockedTurn(t, rt, stub, "block me")
	expect("empty queue", "", steerq.SendNowNotQueued)
	rt.deliverToSession("sys notice")
	expect("only a notice queued", "", steerq.SendNowNotQueued)
	if !rt.enqueueUserTurn("q1", "steer-a", nil) {
		t.Fatal("steer enqueue refused")
	}
	expect("item no longer queued", "q-gone", steerq.SendNowNotQueued)
	rt.replyPending = func() bool { return true }
	expect("turn is a send_message reply", "q1", steerq.SendNowReply)
	rt.replyPending = nil
	expect("named item queued", "q1", steerq.SendNowRecorded)
	expect("any item queued", "", steerq.SendNowRecorded)

	rt.stream.cancelInFlight()
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "sys notice") >= 0 })
	stub.releaseN(2)
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "steer-a") >= 0 })
	stub.releaseN(3)
	waitTurnsPersisted(t, store, dir, a.SessionID, 2)
}

// TestSendNowDeliversTheQueueHead pins what a recorded Send now actually
// delivers when SEVERAL user messages wait: the yield stops the running turn
// at its round boundary, and the queue then drains FIFO — the HEAD runs
// first, which is usually NOT the item the control was on (its id only
// proves the request still has something to deliver; both hosts put the
// button on the newest bubble). The hosts' "Sending…" wait is taken back by
// that first drain, so the head's turn is the observable proof the request
// was consumed — and the rest of the queue must keep following, one turn
// each.
func TestSendNowDeliversTheQueueHead(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	// Round 1 asks for a tool, so the turn reaches a round boundary: a yield
	// is only honored BEFORE the next model call.
	stub.firstTools = []llm.ToolCall{{
		ID:   "call_read",
		Name: "read_file",
		Args: map[string]any{"path": "f.txt"},
	}}
	s, a, store := newContinuationServer(t, stub, dir)
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}
	startBlockedTurn(t, rt, stub, "block me")
	if !rt.enqueueUserTurn("q1", "steer-a", nil) {
		t.Fatal("first steer enqueue refused")
	}
	if !rt.enqueueUserTurn("q2", "steer-b", nil) {
		t.Fatal("second steer enqueue refused")
	}
	// The request names the NEWER item — the bubble that carries the control.
	if got := rt.sendNow("q2"); got != steerq.SendNowRecorded {
		t.Fatalf("sendNow(q2) = %v, want SendNowRecorded", got)
	}
	// Releasing the round in flight is enough for the head to start: the turn
	// stops at the boundary (its tool call still runs) instead of starting
	// another round, and the worker delivers the queue's head.
	stub.releaseN(1)
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "steer-a") >= 0 })
	if userMsgIndex(a, "steer-b") >= 0 {
		t.Fatal("FIFO violated: the newer item ran before the queue's head")
	}
	// The remaining item follows at the head turn's own end, so a Send now on
	// a multi-item queue does not strand the rest.
	stub.releaseN(2)
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "steer-b") >= 0 })
	msgs := a.SnapshotMessages()
	ia, ib := userMsgIndexIn(msgs, "steer-a"), userMsgIndexIn(msgs, "steer-b")
	if ia < 0 || ib < 0 || !(ia < ib) {
		t.Fatalf("delivery order violated: steer-a@%d, steer-b@%d", ia, ib)
	}
	if !hasAssistantBetween(msgs, ia, ib) {
		t.Fatalf("no assistant reply between steer-a@%d and steer-b@%d, want one turn each", ia, ib)
	}
	stub.releaseN(3)
	// Both delivered turns' closing flushes must land before TempDir teardown.
	waitTurnsPersisted(t, store, dir, a.SessionID, 2)
}

// TestQueueSendNowDrainsTheWholeQueue pins the wire contract of the Send now
// button: ONE press asks the running turn to stop at its round boundary, and
// the WHOLE queue then drains FIFO — one turn per user message, each announced
// by the queue_update the clients clean their chips up from (queueLeft names
// the id that started; Content is the text that turn runs). The press names
// the NEWEST item, so the frames also prove it is not a per-item send.
func TestQueueSendNowDrainsTheWholeQueue(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "f.txt"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	stub := newBlockingStub()
	// Round 1 asks for a tool, so the turn reaches a round boundary: a yield
	// is only honored before the next model call.
	stub.firstTools = []llm.ToolCall{{
		ID:   "call_read",
		Name: "read_file",
		Args: map[string]any{"path": "f.txt"},
	}}
	s, a, store := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	conn := dialWS(t, srv, "/ws")
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	// A turn blocking inside the provider, with three messages queued behind
	// it — the ids are the clients' correlation tokens.
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "block me"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	stub.waitBlocked(1)
	items := []struct{ id, text string }{
		{"q-a", "steer-a"}, {"q-b", "steer-b"}, {"q-c", "steer-c"},
	}
	for _, it := range items {
		if err := conn.WriteJSON(WSMessage{Type: "message", Content: it.text, QueueID: it.id}); err != nil {
			t.Fatalf("queue %s: %v", it.id, err)
		}
	}
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "queue_update" && len(m.Queue) == len(items)
	})

	// One press, on the NEWEST item — where both hosts put the control.
	if err := conn.WriteJSON(WSMessage{Type: "queue_send_now", QueueID: "q-c"}); err != nil {
		t.Fatalf("send now: %v", err)
	}
	// Ending each blocked turn is what lets the worker start the next item,
	// and it announces it first. Call 1 is the yielded round; the deliveries
	// are calls 2..4.
	for i, it := range items {
		stub.releaseN(i + 1)
		msg := readUntil(t, conn, 10*time.Second, func(m WSMessage) bool {
			return m.Type == "queue_update" && len(m.QueueLeft) > 0
		})
		if len(msg.QueueLeft) != 1 || msg.QueueLeft[0] != it.id {
			t.Fatalf("delivery %d: queueLeft = %v, want [%s] (FIFO from the head)", i, msg.QueueLeft, it.id)
		}
		if msg.Content != it.text {
			t.Fatalf("delivery %d: started text = %q, want %q", i, msg.Content, it.text)
		}
		if want := len(items) - i - 1; len(msg.Queue) != want {
			t.Fatalf("delivery %d: queue holds %d items, want %d left", i, len(msg.Queue), want)
		}
	}
	stub.releaseN(4)
	// The whole queue ran, in send order, one turn each.
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "steer-c") >= 0 })
	msgs := a.SnapshotMessages()
	ia, ib, ic := userMsgIndexIn(msgs, "steer-a"), userMsgIndexIn(msgs, "steer-b"), userMsgIndexIn(msgs, "steer-c")
	if ia < 0 || ib < 0 || ic < 0 || !(ia < ib && ib < ic) {
		t.Fatalf("delivery order violated: steer-a@%d, steer-b@%d, steer-c@%d", ia, ib, ic)
	}
	waitTurnsPersisted(t, store, dir, a.SessionID, 3)
}

// TestUserQueueCapRejects pins the user-side overflow policy: user text is
// never silently dropped — the enqueue is REFUSED at the cap, and the
// refusal does not disturb already-queued items.
func TestUserQueueCapRejects(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	rt, _ := s.registry.get(a.SessionID)

	startBlockedTurn(t, rt, stub, "block me")
	for i := 0; i < steerq.UserCap; i++ {
		if !rt.enqueueUserTurn("", "steer-"+string(rune('a'+i)), nil) {
			t.Fatalf("enqueue %d refused before cap", i)
		}
	}
	if rt.enqueueUserTurn("", "one too many", nil) {
		t.Fatal("enqueue past the user cap must be refused")
	}
	if got := rt.countUser(); got != steerq.UserCap {
		t.Fatalf("countUser = %d, want %d", got, steerq.UserCap)
	}
	rt.stream.cancelInFlight()
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "steer-a") >= 0 })
	// Tear down cleanly: release every queued turn and wait for the LAST
	// one's final flush so nothing races the TempDir teardown.
	for i := 2; i <= steerq.UserCap+1; i++ {
		stub.releaseN(i)
	}
	waitTurnsPersisted(t, store, dir, a.SessionID, steerq.UserCap)
}

// TestCancelClearsUserQueueKeepsSystem pins the interrupt semantics:
// clearing the user queue on interrupt removes ONLY user items — system
// deliveries are machine bookkeeping and keep draining.
func TestCancelClearsUserQueueKeepsSystem(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	rt, _ := s.registry.get(a.SessionID)

	startBlockedTurn(t, rt, stub, "block me")
	if !rt.enqueueUserTurn("q1", "steer-a", nil) || !rt.enqueueUserTurn("q2", "steer-b", nil) {
		t.Fatal("steer enqueue refused")
	}
	rt.deliverToSession("sys notice")

	if n := rt.clearUserQueue(); n != 2 {
		t.Fatalf("clearUserQueue removed %d items, want 2", n)
	}
	if n := rt.clearUserQueue(); n != 0 {
		t.Fatalf("second clear removed %d items, want 0", n)
	}
	// The system delivery survived the interrupt: it drains as the next
	// turn (stub stream call 2, released so the turn completes).
	rt.stream.cancelInFlight()
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "sys notice") >= 0 })
	if userMsgIndex(a, "steer-a") >= 0 || userMsgIndex(a, "steer-b") >= 0 {
		t.Fatal("interrupt must drop user steering messages")
	}
	stub.releaseN(2)
	waitTurnsPersisted(t, store, dir, a.SessionID, 1)
}

// TestRemoveQueuedItem pins per-item cancel: one user item by id, leaving
// its neighbors queued in order.
func TestRemoveQueuedItem(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	rt, _ := s.registry.get(a.SessionID)

	startBlockedTurn(t, rt, stub, "block me")
	for _, id := range []string{"q1", "q2", "q3"} {
		if !rt.enqueueUserTurn(id, "steer-"+id, nil) {
			t.Fatalf("enqueue %s refused", id)
		}
	}
	if rt.removeQueuedItem("nope") {
		t.Fatal("removing an unknown id must report false")
	}
	if !rt.removeQueuedItem("q2") {
		t.Fatal("removing a queued id must report true")
	}
	// A removed id cannot be removed twice.
	if rt.removeQueuedItem("q2") {
		t.Fatal("double remove must report false")
	}
	rt.stream.cancelInFlight()
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "steer-q1") >= 0 })
	stub.releaseN(2)
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "steer-q3") >= 0 })
	if userMsgIndex(a, "steer-q2") >= 0 {
		t.Fatal("removed queue item must not be delivered")
	}
	stub.releaseN(3)
	waitTurnsPersisted(t, store, dir, a.SessionID, 2)
}

// TestSystemOverflowSparesUserItems pins the per-kind cap: a system
// overflow drops the oldest SYSTEM delivery, never a queued user message.
func TestSystemOverflowSparesUserItems(t *testing.T) {
	rt := &sessionRuntime{agent: &agent.Agent{}, deliverNotify: make(chan struct{}, 1)}
	rt.deliverWorker.Store(true) // no worker goroutine: the queue stays put
	for i := 0; i < steerq.UserCap; i++ {
		if !rt.enqueueUserTurn("", "user-"+string(rune('a'+i)), nil) {
			t.Fatalf("user enqueue %d refused", i)
		}
	}
	for i := 0; i < steerq.SystemCap+2; i++ {
		rt.deliverToSession("sys-" + string(rune('a'+i)))
	}
	// Every user item survives the system burst.
	if got := rt.countUser(); got != steerq.UserCap {
		t.Fatalf("countUser = %d after system overflow, want %d (user items must not be dropped)", got, steerq.UserCap)
	}
	// System items are bounded at the cap.
	sys := 0
	for _, it := range queuedItems(rt) {
		if it.Kind != steerq.User {
			sys++
		}
	}
	if sys != steerq.SystemCap {
		t.Fatalf("queued system items = %d, want %d", sys, steerq.SystemCap)
	}
}

// TestSteerableInputWords pins the command-word classification the steering
// branch runs BEFORE taking the turn lock (agent.IsCommandInput, shared with
// the TUI's mid-turn gate): command-shaped input keeps the dispatch/busy
// path, chat text queues, and a word that merely STARTS with a command word
// stays chat. An EXACT-only command word followed by arguments is chat too —
// HandleHelpCommand and friends match the whole trimmed input, so
// "help me debug" reaches the model rather than a usage message.
func TestSteerableInputWords(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		want bool
	}{
		{"plain chat", "fix the parser bug", true},
		{"bare help is a command", "help", false},
		{"exact-only command with args is chat", "help me debug", true},
		{"tui-only command word counts everywhere", "compact", false},
		{"word starting with switch", "switchboard is down", true},
		{"slash help", "/help", false},
		{"slash anything", "/new", false},
		{"bare plan", "plan", false},
		{"bare think", "think", false},
		{"think with level", "think high", false},
		{"bare models", "models", false},
		{"model selector", "models gpt-4o", false},
		{"bare resume", "resume", false},
		{"resume arg", "resume latest", false},
		{"word starting with new", "news about the build", true},
		{"word starting with resume", "resumes are attached", true},
		{"word starting with fork", "forklift is broken", true},
		{"word starting with models", "modelsx is not a command", true},
		{"word starting with think", "thinker needed", true},
		{"empty", "", false},
		{"whitespace", "   ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := isPlainChatInput(tc.in); got != tc.want {
				t.Fatalf("isPlainChatInput(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
	// steerableInput: text is classified, an image-only message is steerable,
	// and a message with neither is not.
	if !steerableInput("", 1) {
		t.Fatal("image-only message must be steerable")
	}
	if steerableInput("", 0) {
		t.Fatal("empty message must not be steerable")
	}
	if !steerableInput("hello", 2) {
		t.Fatal("chat with images must be steerable")
	}
	if steerableInput("/new", 1) {
		t.Fatal("command with images must not be steerable")
	}
}

// TestQueueIDValidationAndDedupe pins the queue-id policy: a usable
// client id is kept (it correlates the sender's optimistic bubble), while a
// missing, oversized, malformed, or DUPLICATE id gets a server-generated one
// — two queued items sharing an id would collapse into one entry in every
// client's per-id bubble map.
func TestQueueIDValidationAndDedupe(t *testing.T) {
	rt := &sessionRuntime{agent: &agent.Agent{}, deliverNotify: make(chan struct{}, 1)}
	rt.deliverWorker.Store(true) // no worker goroutine: the queue stays put

	for _, id := range []string{"q-keep", "q-keep", "", strings.Repeat("x", maxQueueIDLen+1), "bad id!", "q-ok_2.x"} {
		if !rt.enqueueUserTurn(id, "text-"+id, nil) {
			t.Fatalf("enqueue %q refused", id)
		}
	}
	items := queuedItems(rt)
	if len(items) != 6 {
		t.Fatalf("queued items = %d, want 6", len(items))
	}
	if items[0].ID != "q-keep" {
		t.Fatalf("first item id = %q, want the client's q-keep", items[0].ID)
	}
	if items[5].ID != "q-ok_2.x" {
		t.Fatalf("token-shaped id = %q, want it kept verbatim", items[5].ID)
	}
	seen := map[string]bool{}
	for i, it := range items {
		if seen[it.ID] {
			t.Fatalf("duplicate queue id %q at item %d", it.ID, i)
		}
		seen[it.ID] = true
	}
	if !strings.HasPrefix(items[1].ID, "q_") {
		t.Fatalf("duplicate id was not replaced: %q", items[1].ID)
	}
	if validQueueID("") || validQueueID(strings.Repeat("x", maxQueueIDLen+1)) || validQueueID("bad id!") {
		t.Fatal("validQueueID accepted an unusable id")
	}
}

// TestImageOnlyMessageSteersWhileBusy pins the classification end-to-end: a
// message carrying attachments and no text is QUEUED (the image is its
// content — there is no text to classify) instead of getting the busy
// rejection, and the drained turn receives the image.
func TestImageOnlyMessageSteersWhileBusy(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}
	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	startBlockedTurn(t, rt, stub, "block me")
	img := llm.ImageInput{DataURL: "data:image/png;base64,AAAA", Detail: "high"}
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "", Images: []llm.ImageInput{img}, QueueID: "q-imgonly"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	frame := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "queue_update" || m.Type == "response"
	})
	if frame.Type != "queue_update" || len(frame.Queue) != 1 || frame.Queue[0].ID != "q-imgonly" {
		t.Fatalf("image-only message was not queued: %+v", frame)
	}
	// Drain: the item runs as its own turn, carrying the image.
	rt.stream.cancelInFlight()
	waitFor(t, 5*time.Second, func() bool {
		for _, m := range a.SnapshotMessages() {
			if m.Role == "user" && len(m.Images) == 1 {
				return true
			}
		}
		return false
	})
	stub.releaseN(2)
	waitTurnsPersisted(t, store, dir, a.SessionID, 1)
}

// TestSteerQueueFullRevokesOptimisticBubble pins the queue-full client
// contract: the refusal answers on the conversation channel AND broadcasts the
// (unchanged) queue state, so the sending tab's optimistic chip is revoked
// instead of lingering as a phantom "queued" entry.
func TestSteerQueueFullRevokesOptimisticBubble(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}
	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	startBlockedTurn(t, rt, stub, "block me")
	for i := 0; i < steerq.UserCap; i++ {
		if !rt.enqueueUserTurn("", "fill-"+string(rune('a'+i)), nil) {
			t.Fatalf("enqueue %d refused before the cap", i)
		}
		// Each fill broadcast its own queue_update: consume it so the frames
		// below are unambiguously the refused message's.
		fill := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "queue_update" })
		if len(fill.Queue) != i+1 {
			t.Fatalf("fill %d queue_update = %d items, want %d", i, len(fill.Queue), i+1)
		}
	}
	// The refused send carries a correlation id: it must appear in the queue
	// frames that follow only as an ABSENT id (the client's "removed before
	// running" signal).
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "one too many", QueueID: "q-phantom"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		return m.Type == "response" || m.Type == "queue_update"
	})
	if resp.Type != "response" || !strings.Contains(resp.Content, "too many queued messages") {
		t.Fatalf("expected the queue-full rejection first, got %+v", resp)
	}
	if resp.QueueID != "q-phantom" {
		t.Fatalf("rejection queueId = %q, want the refused send's q-phantom", resp.QueueID)
	}
	qu := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "queue_update" })
	if len(qu.Queue) != steerq.UserCap {
		t.Fatalf("queue_update queue = %d items, want the unchanged %d", len(qu.Queue), steerq.UserCap)
	}
	for _, it := range qu.Queue {
		if it.ID == "q-phantom" {
			t.Fatal("the refused item must not be queued")
		}
	}
	if len(qu.QueueLeft) != 0 {
		t.Fatalf("queueLeft = %v, want none (the refused item was never queued)", qu.QueueLeft)
	}
	// Drain the queue so the blocked turn's followers land before teardown.
	rt.stream.cancelInFlight()
	waitFor(t, 5*time.Second, func() bool { return userMsgIndex(a, "fill-a") >= 0 })
	for i := 2; i <= steerq.UserCap+1; i++ {
		stub.releaseN(i)
	}
	waitTurnsPersisted(t, store, dir, a.SessionID, steerq.UserCap)
}

// TestBusyRejectionEchoesQueueID pins that a command refused because another
// client's turn holds the session names the refused send, so the sender can
// tell it apart from the running turn's own replies.
func TestBusyRejectionEchoesQueueID(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, a, store := newContinuationServer(t, stub, dir)
	rt, ok := s.registry.get(a.SessionID)
	if !ok {
		t.Fatal("default runtime not registered")
	}
	srv := startWSServer(t, s)
	defer srv.Close()
	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })

	startBlockedTurn(t, rt, stub, "block me")
	if err := conn.WriteJSON(WSMessage{Type: "message", Content: "/mode plan", QueueID: "q-busy"}); err != nil {
		t.Fatalf("send: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "response" })
	if resp.Content != errAgentBusy {
		t.Fatalf("response = %q, want the busy rejection", resp.Content)
	}
	if resp.QueueID != "q-busy" {
		t.Fatalf("busy rejection queueId = %q, want q-busy", resp.QueueID)
	}
	stub.releaseN(1)
	waitFor(t, 10*time.Second, func() bool {
		snap, err := store.LoadInWorkingDir(dir, a.SessionID)
		if err != nil {
			return false
		}
		for _, m := range snap.Messages {
			if m.Role == "assistant" && m.Content == "headless-done" {
				return true
			}
		}
		return false
	})
}
