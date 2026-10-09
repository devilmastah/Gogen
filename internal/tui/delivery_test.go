package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"gogen/internal/agent"
)

// newQueueTestModel builds a minimal Model with a live-sessions registry:
// the steering queue is per-session state on liveSession (steering.go), so
// every queue test resolves items through the focused session. The turn
// commands are never EXECUTED here (they would drive the zero agent); the
// tests assert queue state and the returned command's existence.
func newQueueTestModel() *Model {
	a := &agent.Agent{}
	m := &Model{agent: a, ctx: context.Background(), keys: DefaultKeyMap, textarea: textarea.New()}
	m.lives = newLiveSessions(a)
	m.viewport.Width = 200
	return m
}

// keyPress builds a KeyPressMsg from a name, the same helper shape the
// other TUI key tests use.
func keyPress(s string) tea.KeyPressMsg {
	switch s {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}
	case "d":
		return tea.KeyPressMsg{Code: 'd'}
	case "D":
		return tea.KeyPressMsg{Code: 'D'}
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}
	}
	return tea.KeyPressMsg{Code: rune(s[0])}
}

// TestDrainDeliveriesOnlyWhenIdle pins the TUI drain guard: the focused
// session's queue is drained only while no turn is streaming. A contiguous
// run of system notices drains as ONE turn (one provider request), so both
// items leave the queue together; the next drain waits for that turn to end.
func TestDrainDeliveriesOnlyWhenIdle(t *testing.T) {
	m := newQueueTestModel()
	s := m.focusedSession()
	s.steerQueue = []steerItem{
		{id: "a", text: "notice-a", system: true},
		{id: "b", text: "notice-b", system: true},
	}

	m.streaming = true
	if cmd := m.drainDeliveries(); cmd != nil {
		t.Fatal("must not drain while streaming")
	}
	if len(s.steerQueue) != 2 {
		t.Fatalf("queue mutated while streaming: %v", s.steerQueue)
	}

	m.streaming = false
	if cmd := m.drainDeliveries(); cmd == nil {
		t.Fatal("must drain when idle")
	}
	if len(s.steerQueue) != 0 {
		t.Fatalf("queue = %v, want empty (the system run drains as one turn)", s.steerQueue)
	}
	if !m.streaming {
		t.Fatal("drain must mark the focused session as streaming")
	}

	// Still streaming (the just-started turn) → a further item waits.
	if cmd := m.drainDeliveries(); cmd != nil {
		t.Fatal("further items must wait for the just-started turn to end")
	}
	// An empty queue yields no command even when idle.
	m.streaming = false
	s.steerQueue = nil
	if cmd := m.drainDeliveries(); cmd != nil {
		t.Fatal("empty queue must not drain")
	}
}

// TestSystemDeliveriesBatchOneTurn pins the TUI batch boundaries: a
// contiguous run of system notices drains as ONE turn, a USER steering item
// keeps its own turn, and a system item behind the user item is not jumped
// (FIFO across kinds).
func TestSystemDeliveriesBatchOneTurn(t *testing.T) {
	m := newQueueTestModel()
	s := m.focusedSession()
	s.steerQueue = []steerItem{
		{id: "s1", text: "notice-1", system: true},
		{id: "s2", text: "notice-2", system: true},
		{id: "u1", text: "user-draft"},
		{id: "s3", text: "notice-3", system: true},
	}

	// The two leading notices drain as one turn.
	if cmd := m.drainSessionQueue(s); cmd == nil {
		t.Fatal("batch drain must start a turn")
	}
	if len(s.steerQueue) != 2 || s.steerQueue[0].id != "u1" || s.steerQueue[1].id != "s3" {
		t.Fatalf("after batch drain queue = %v, want [u1, s3]", s.steerQueue)
	}

	// Simulate the batched turn ending: drainSessionQueue only starts idle
	// sessions, and the command is never executed in this test.
	s.streaming = false
	m.streaming = false

	// The user item keeps its own turn...
	if cmd := m.drainSessionQueue(s); cmd == nil {
		t.Fatal("user drain must start a turn")
	}
	if len(s.steerQueue) != 1 || s.steerQueue[0].id != "s3" {
		t.Fatalf("after user drain queue = %v, want [s3]", s.steerQueue)
	}

	s.streaming = false
	m.streaming = false
	// ...and the system item behind it drains alone.
	if cmd := m.drainSessionQueue(s); cmd == nil {
		t.Fatal("trailing system drain must start a turn")
	}
	if len(s.steerQueue) != 0 {
		t.Fatalf("queue = %v, want empty", s.steerQueue)
	}
}

// TestDeliveryRequestMsgQueues pins the Update-thread entry: a
// deliveryRequestMsg lands as a SYSTEM item on the focused session's
// queue and drains immediately when idle.
func TestDeliveryRequestMsgQueues(t *testing.T) {
	m := newQueueTestModel()
	m.streaming = true

	model, cmd := m.handleMsg(deliveryRequestMsg{text: "incoming notice"})
	m2 := model.(*Model)
	s := m2.focusedSession()
	if len(s.steerQueue) != 1 || s.steerQueue[0].text != "incoming notice" || !s.steerQueue[0].system {
		t.Fatalf("queue = %v, want one system item [incoming notice]", s.steerQueue)
	}
	if cmd != nil {
		t.Fatal("must not start a turn while streaming")
	}

	// Idle: the queued delivery drains as its own turn.
	m2.streaming = false
	if _, cmd := m2.handleMsg(deliveryRequestMsg{text: "second notice"}); cmd == nil {
		t.Fatal("must drain when idle")
	}
}

// TestDeliveryRequestMsgRoutesBySid pins the per-session notice routing: the
// notice names the session it was raised for, so a focus switch between the
// hook firing (background goroutine) and this handler cannot land it on the
// focused session — and a background session's notice drains on ITS OWN idle
// boundary, not the focused session's.
func TestDeliveryRequestMsgRoutesBySid(t *testing.T) {
	m := newQueueTestModel()
	bg := m.lives.Add(&agent.Agent{}, "bg")

	// 1. A BUSY background target only queues (its own turn is in flight).
	bg.streaming = true
	model, cmd := m.handleMsg(deliveryRequestMsg{sid: bg.id, text: "busy notice"})
	m2 := model.(*Model)
	if cmd != nil {
		t.Fatal("must not start a turn on a streaming session")
	}
	if len(bg.steerQueue) != 1 || bg.steerQueue[0].text != "busy notice" || !bg.steerQueue[0].system {
		t.Fatalf("background queue = %v, want one system item", bg.steerQueue)
	}
	if q := m2.focusedSession().steerQueue; len(q) != 0 {
		t.Fatalf("notice landed on the focused session's queue: %v", q)
	}

	// 2. An IDLE background target drains its queue as ONE batched turn
	// immediately (the still-queued notice runs with the new one), leaving
	// the focused session's queue alone.
	bg.streaming = false
	model, cmd = m2.handleMsg(deliveryRequestMsg{sid: bg.id, text: "idle notice"})
	m3 := model.(*Model)
	if cmd == nil {
		t.Fatal("an idle background session must drain its own notice")
	}
	if !bg.streaming {
		t.Fatal("the drained turn must mark the background session as streaming")
	}
	if len(bg.steerQueue) != 0 {
		t.Fatalf("background queue = %v, want empty (the system run batches into one turn)", bg.steerQueue)
	}
	if q := m3.focusedSession().steerQueue; len(q) != 0 {
		t.Fatalf("focused session queue = %v, want empty", q)
	}

	// 3. An unknown sid (a slot closed since the notice fired) falls back to
	// the focused session rather than dropping the notice. The focused
	// session is busy here, so the notice queues instead of draining.
	m3.streaming = true
	model, cmd = m3.handleMsg(deliveryRequestMsg{sid: "gone", text: "late notice"})
	m4 := model.(*Model)
	if cmd != nil {
		t.Fatal("a busy focused session must not start a turn")
	}
	fq := m4.focusedSession().steerQueue
	if len(fq) != 1 || fq[0].text != "late notice" {
		t.Fatalf("focused queue = %v, want the late notice", fq)
	}
}

// TestDeliveryQueueOverflowDropsOldest pins the TUI system-delivery cap:
// overflow drops the OLDEST SYSTEM item (freshness wins) and never a
// queued user message; the drop is reported at the next drain.
func TestDeliveryQueueOverflowDropsOldest(t *testing.T) {
	m := newQueueTestModel()
	m.streaming = true
	s := m.focusedSession()
	// One user item must survive the system overflow untouched.
	s.steerQueue = append(s.steerQueue, steerItem{id: "u1", text: "user draft"})
	for i := 1; i <= maxPendingDeliveries; i++ {
		s.steerQueue = append(s.steerQueue, steerItem{id: fmt.Sprintf("n%d", i), text: fmt.Sprintf("notice-%d", i), system: true})
	}

	model, cmd := m.handleMsg(deliveryRequestMsg{text: "notice-9"})
	m2 := model.(*Model)
	s2 := m2.focusedSession()
	if cmd != nil {
		t.Fatal("must not drain while streaming")
	}
	// The oldest SYSTEM item (notice-1) is gone; the user item and
	// notice-2..9 remain.
	sys := 0
	hasUser, hasN1 := false, false
	for _, it := range s2.steerQueue {
		if it.system {
			sys++
			if it.text == "notice-1" {
				hasN1 = true
			}
			continue
		}
		if it.text == "user draft" {
			hasUser = true
		}
	}
	if sys != maxPendingDeliveries {
		t.Fatalf("queued system items = %d, want %d", sys, maxPendingDeliveries)
	}
	if hasN1 {
		t.Fatal("overflow must drop the OLDEST system delivery")
	}
	if !hasUser {
		t.Fatal("system overflow must never drop a user steering item")
	}
	if m2.deliveryDrops != 1 {
		t.Fatalf("deliveryDrops = %d, want 1", m2.deliveryDrops)
	}

	// Idle drain: the drop is reported first, then the next item starts.
	m2.streaming = false
	if cmd := m2.drainDeliveries(); cmd == nil {
		t.Fatal("must drain when idle")
	}
	if m2.deliveryDrops != 0 {
		t.Fatal("drop counter must reset after rendering")
	}
	// The drop report precedes the drained turn's user line in the chat.
	joined := strings.Join(m2.chatLines[len(m2.chatLines)-2:], "\n")
	if !strings.Contains(joined, "dropped") {
		t.Fatalf("drop report missing: %q", joined)
	}
}

// TestSubmitQueuesWhileBusy pins the steering entry point: Enter with the
// session busy queues the text (FIFO), clears the composer, reports the
// queue depth, and does NOT start a turn.
func TestSubmitQueuesWhileBusy(t *testing.T) {
	m := newQueueTestModel()
	m.streaming = true
	m.textarea.SetValue("fix the tests")

	model, _, _ := m.handleSubmitKey(keyPress("enter"))
	m2 := model.(*Model)
	s := m2.focusedSession()
	if len(s.steerQueue) != 1 || s.steerQueue[0].text != "fix the tests" || s.steerQueue[0].system {
		t.Fatalf("queue = %v, want one user item [fix the tests]", s.steerQueue)
	}
	if m2.textarea.Value() != "" {
		t.Fatalf("composer not cleared: %q", m2.textarea.Value())
	}
	if !strings.Contains(m2.statusMsg, "Queued (1)") {
		t.Fatalf("statusMsg = %q, want queue acknowledgment", m2.statusMsg)
	}
	// FIFO across kinds: a queued delivery lands behind the user item.
	model, _ = m2.handleMsg(deliveryRequestMsg{text: "job notice"})
	m3 := model.(*Model)
	s3 := m3.focusedSession()
	if len(s3.steerQueue) != 2 || s3.steerQueue[0].system || !s3.steerQueue[1].system {
		t.Fatalf("queue order = %v, want [user, system]", s3.steerQueue)
	}
	// Draining pops strictly in arrival order (user first, then system).
	m3.streaming = false
	if cmd := m3.drainSessionQueue(s3); cmd == nil {
		t.Fatal("drain must start the queued turn")
	}
	if s3.steerQueue[0].text != "job notice" {
		t.Fatalf("after first drain queue = %v, want [job notice]", s3.steerQueue)
	}
}

// TestUserQueueCapRefuses pins the TUI user cap: overflow REFUSES at
// submit (typed text is never silently dropped) and leaves the queue
// untouched.
func TestUserQueueCapRefuses(t *testing.T) {
	m := newQueueTestModel()
	m.streaming = true
	s := m.focusedSession()
	for i := 0; i < maxSteerUserQueue; i++ {
		s.steerQueue = append(s.steerQueue, steerItem{id: fmt.Sprintf("u%d", i), text: fmt.Sprintf("msg-%d", i)})
	}
	m.textarea.SetValue("one too many")
	model, _, _ := m.handleSubmitKey(keyPress("enter"))
	m2 := model.(*Model)
	if got := s.countUserSteer(); got != maxSteerUserQueue {
		t.Fatalf("countUserSteer = %d, want %d", got, maxSteerUserQueue)
	}
	if !strings.Contains(m2.statusMsg, "Too many queued messages") {
		t.Fatalf("statusMsg = %q, want cap rejection", m2.statusMsg)
	}
}

// TestCancelClearsUserSteer pins the interrupt interaction: ctrl+c on a
// busy session drops the user's queued items (reported) and KEEPS system
// deliveries.
func TestCancelClearsUserSteer(t *testing.T) {
	m := newQueueTestModel()
	m.streaming = true
	s := m.focusedSession()
	s.steerQueue = []steerItem{
		{id: "u1", text: "queued user 1"},
		{id: "s1", text: "queued delivery", system: true},
		{id: "u2", text: "queued user 2"},
	}

	model, _, _ := m.handleCancelKey()
	m2 := model.(*Model)
	s2 := m2.focusedSession()
	if len(s2.steerQueue) != 1 || s2.steerQueue[0].text != "queued delivery" {
		t.Fatalf("queue after cancel = %v, want [queued delivery]", s2.steerQueue)
	}
	last := m2.chatLines[len(m2.chatLines)-1]
	if !strings.Contains(last, "Cancelled") || !strings.Contains(last, "2 queued message(s) removed") {
		t.Fatalf("cancel report = %q, want dropped count", last)
	}
}

// TestQueueModal pins the /queue management modal: render with cursor,
// per-item delete (user only), clear-all, esc close.
func TestQueueModal(t *testing.T) {
	m := newQueueTestModel()
	s := m.focusedSession()
	s.steerQueue = []steerItem{
		{id: "u1", text: "first queued message"},
		{id: "s1", text: "a system delivery", system: true},
		{id: "u2", text: "third queued message"},
	}
	m.modal = ModalQueue

	// Cursor defaults to the first row; render shows all three.
	m.queueCursor = 0
	rendered := m.renderQueueModal()
	for _, want := range []string{"first queued message", "[delivery] a system delivery", "third queued message"} {
		if !strings.Contains(rendered, want) {
			t.Fatalf("modal render missing %q:\n%s", want, rendered)
		}
	}

	// `d` on a USER row removes that item.
	model, _ := m.handleQueueKey(keyPress("d"))
	m2 := model.(*Model)
	s2 := m2.focusedSession()
	if len(s2.steerQueue) != 2 || s2.steerQueue[0].text != "a system delivery" {
		t.Fatalf("queue after d = %v", s2.steerQueue)
	}
	// `d` on the SYSTEM row is refused (machine bookkeeping).
	m2.queueCursor = 0
	model, _ = m2.handleQueueKey(keyPress("d"))
	m3 := model.(*Model)
	if len(m3.focusedSession().steerQueue) != 2 {
		t.Fatal("system delivery must not be deletable")
	}
	// `D` clears every user item, keeps the system one.
	model, _ = m3.handleQueueKey(keyPress("D"))
	m4 := model.(*Model)
	s4 := m4.focusedSession()
	if len(s4.steerQueue) != 1 || !s4.steerQueue[0].system {
		t.Fatalf("queue after D = %v, want [system]", s4.steerQueue)
	}
	// esc closes the modal.
	model, _ = m4.handleQueueKey(keyPress("esc"))
	if model.(*Model).modal != ModalNone {
		t.Fatal("esc must close the queue modal")
	}
}

// TestQueueModalCursorClampsAfterShrink pins the stale-cursor guard: the
// queue can shrink while the modal is open — a drain pops the head at every
// turn boundary (the modal opens while a turn runs) and an interrupt clears
// the user's items — leaving the cursor past the end. The next keypress must
// clamp before indexing ("d" used to panic with an index-out-of-range).
func TestQueueModalCursorClampsAfterShrink(t *testing.T) {
	m := newQueueTestModel()
	s := m.focusedSession()
	s.steerQueue = []steerItem{
		{id: "u1", text: "first"},
		{id: "u2", text: "second"},
	}
	m.modal = ModalQueue
	m.queueCursor = 1 // points at "second"

	// The turn boundary's drain pops the head while the modal is open.
	s.steerQueue = s.steerQueue[1:]

	// `d` with the stale cursor must clamp, then delete the item under the
	// clamped cursor — not panic.
	model, _ := m.handleQueueKey(keyPress("d"))
	m2 := model.(*Model)
	if got := m2.focusedSession().steerQueue; len(got) != 0 {
		t.Fatalf("queue after stale-cursor d = %v, want empty", got)
	}
	if m2.queueCursor != 0 {
		t.Fatalf("cursor after emptying = %d, want 0", m2.queueCursor)
	}

	// The clamp also covers a tail shrink: the cursor re-points at the last
	// surviving row instead of highlighting nothing.
	m3 := newQueueTestModel()
	s3 := m3.focusedSession()
	s3.steerQueue = []steerItem{
		{id: "u1", text: "first"},
		{id: "u2", text: "second"},
		{id: "u3", text: "third"},
	}
	m3.modal = ModalQueue
	m3.queueCursor = 2
	s3.steerQueue = s3.steerQueue[:2] // "third" left the queue another way
	rendered := m3.renderQueueModal()
	if !strings.Contains(rendered, "> second") {
		t.Fatalf("clamped highlight missing:\n%s", rendered)
	}
}

// TestQueuedSuffixRenders pins the input band's queued count: the progress
// strip (and the compacting strip) carry "· N queued" while items wait.
func TestQueuedSuffixRenders(t *testing.T) {
	m := newQueueTestModel()
	m.streaming = true
	m.progressPhase = progressActive
	m.spinner = newProgressSpinner()
	s := m.focusedSession()
	s.steerQueue = []steerItem{{id: "u1", text: "one"}, {id: "u2", text: "two"}}

	got := m.renderProgressInput()
	if !strings.Contains(got, "· 2 queued") {
		t.Fatalf("progress strip missing queue count: %q", got)
	}
	// The strip is ONE row (the composer renders below it — the band's
	// total height is unchanged; SetSize reserves the strip's row).
	if h := lipgloss.Height(got); h != 1 {
		t.Fatalf("progress strip height=%d, want 1", h)
	}

	// The compacting strip carries the count too.
	m.progressPhase = progressHidden
	got = m.renderCompactingInput()
	if !strings.Contains(got, "· 2 queued") {
		t.Fatalf("compacting strip missing queue count: %q", got)
	}

	// The suffix reads the focused session's queue: empty → no suffix.
	s.steerQueue = nil
	if got := m.renderProgressInput(); strings.Contains(got, "queued") {
		t.Fatalf("empty queue must not render a suffix: %q", got)
	}
}

// TestTurnEndRestoresIdleComposerPlaceholder pins the placeholder swap at
// the terminal boundary: a finished turn with NOTHING queued restores the
// idle placeholder (the queued one is applied by startTurn while busy, and
// re-applied by the drain when a queued item starts the next turn — the
// swap must therefore run AFTER the streaming flag clears).
func TestTurnEndRestoresIdleComposerPlaceholder(t *testing.T) {
	m := newQueueTestModel()
	m.textarea.Focus()
	m.streaming = true
	m.applyComposerPlaceholder()
	if m.textarea.Placeholder != queuedComposerPlaceholder {
		t.Fatalf("busy placeholder = %q, want %q", m.textarea.Placeholder, queuedComposerPlaceholder)
	}

	// Nothing queued: the turn end flips the placeholder back to idle.
	m.finishFocusedTurn(nil)
	if m.streaming {
		t.Fatal("turn end must clear streaming")
	}
	if m.textarea.Placeholder != idleComposerPlaceholder {
		t.Fatalf("placeholder after idle turn end = %q, want %q", m.textarea.Placeholder, idleComposerPlaceholder)
	}

	// A queued item drains at the same boundary: still busy → the queued
	// placeholder stays.
	m2 := newQueueTestModel()
	m2.textarea.Focus()
	m2.streaming = true
	m2.applyComposerPlaceholder()
	m2.focusedSession().steerQueue = []steerItem{{id: "u1", text: "next"}}
	m2.finishFocusedTurn(nil)
	if !m2.streaming {
		t.Fatal("the drained item must have started its turn")
	}
	if m2.textarea.Placeholder != queuedComposerPlaceholder {
		t.Fatalf("placeholder with a drained turn = %q, want %q", m2.textarea.Placeholder, queuedComposerPlaceholder)
	}
}
