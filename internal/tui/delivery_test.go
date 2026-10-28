package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"charm.land/bubbles/v2/textarea"
	"charm.land/lipgloss/v2"

	tea "charm.land/bubbletea/v2"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/llm/llmtest"
	"gogen/internal/steerq"
)

// newQueueTestModel builds a minimal Model with a live-sessions registry:
// the steering queue is per-session state on liveSession (steering.go), so
// every queue test resolves items through the focused session. The turn
// commands are never EXECUTED here (they would drive the zero agent); the
// tests assert queue state and the returned command's existence.
func newQueueTestModel() *Model {
	return newQueueTestModelFor(&agent.Agent{})
}

// newQueueTestModelFor is newQueueTestModel around a given agent.
func newQueueTestModelFor(a *agent.Agent) *Model {
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
	s.steerQ = testQueue(
		notice("a", "notice-a"),
		notice("b", "notice-b"),
	)

	m.streaming = true
	if cmd := m.drainDeliveries(); cmd != nil {
		t.Fatal("must not drain while streaming")
	}
	if s.steerQ.Len() != 2 {
		t.Fatalf("queue mutated while streaming: %v", s.steerQ)
	}

	m.streaming = false
	if cmd := m.drainDeliveries(); cmd == nil {
		t.Fatal("must drain when idle")
	}
	if s.steerQ.Len() != 0 {
		t.Fatalf("queue = %v, want empty (the system run drains as one turn)", s.steerQ)
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
	s.steerQ = nil
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
	s.steerQ = testQueue(
		notice("s1", "notice-1"),
		notice("s2", "notice-2"),
		userItem("u1", "user-draft"),
		notice("s3", "notice-3"),
	)

	// The two leading notices drain as one turn.
	if cmd := m.drainSessionQueue(s); cmd == nil {
		t.Fatal("batch drain must start a turn")
	}
	if s.steerQ.Len() != 2 || s.steerQ.Items()[0].ID != "u1" || s.steerQ.Items()[1].ID != "s3" {
		t.Fatalf("after batch drain queue = %v, want [u1, s3]", s.steerQ)
	}

	// Simulate the batched turn ending: drainSessionQueue only starts idle
	// sessions, and the command is never executed in this test.
	s.streaming = false
	m.streaming = false

	// The user item keeps its own turn...
	if cmd := m.drainSessionQueue(s); cmd == nil {
		t.Fatal("user drain must start a turn")
	}
	if s.steerQ.Len() != 1 || s.steerQ.Items()[0].ID != "s3" {
		t.Fatalf("after user drain queue = %v, want [s3]", s.steerQ)
	}

	s.streaming = false
	m.streaming = false
	// ...and the system item behind it drains alone.
	if cmd := m.drainSessionQueue(s); cmd == nil {
		t.Fatal("trailing system drain must start a turn")
	}
	if s.steerQ.Len() != 0 {
		t.Fatalf("queue = %v, want empty", s.steerQ)
	}
}

// TestUserRunFollowsJoinQueued pins the TUI side of join_queued: a run of
// queued user messages drains one per turn by default, and as one turn when
// the setting is on. The delivery behind the run is never joined into it.
func TestUserRunFollowsJoinQueued(t *testing.T) {
	for _, tc := range []struct {
		name string
		cfg  *config.Config
		left []string
	}{
		{"off by default", nil, []string{"u2", "s1"}},
		{"on", &config.Config{JoinQueued: "on"}, []string{"s1"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newQueueTestModel()
			m.cfg = tc.cfg
			s := m.focusedSession()
			s.steerQ = testQueue(userItem("u1", "first"), userItem("u2", "second"), notice("s1", "notice"))
			if cmd := m.drainSessionQueue(s); cmd == nil {
				t.Fatal("drain must start a turn")
			}
			var ids []string
			for _, it := range s.steerQ.Items() {
				ids = append(ids, it.ID)
			}
			if strings.Join(ids, ",") != strings.Join(tc.left, ",") {
				t.Fatalf("queue after drain = %v, want %v", ids, tc.left)
			}
		})
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
	if s.steerQ.Len() != 1 || s.steerQ.Items()[0].Text != "incoming notice" || s.steerQ.Items()[0].Kind == steerq.User {
		t.Fatalf("queue = %v, want one system item [incoming notice]", s.steerQ)
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
	if q := bg.steerQ.Items(); len(q) != 1 || q[0].Text != "busy notice" || q[0].Kind == steerq.User {
		t.Fatalf("background queue = %v, want one system item", q)
	}
	if m2.focusedSession().steerQ.Len() != 0 {
		t.Fatalf("notice landed on the focused session's queue: %v", m2.focusedSession().steerQ.Items())
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
	if bg.steerQ.Len() != 0 {
		t.Fatalf("background queue = %v, want empty (the system run batches into one turn)", bg.steerQ)
	}
	if q := m3.focusedSession().steerQ; q.Len() != 0 {
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
	fq := m4.focusedSession().steerQ
	if q := fq.Items(); len(q) != 1 || q[0].Text != "late notice" {
		t.Fatalf("focused queue = %v, want the late notice", q)
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
	s.queue().Append(userItem("u1", "user draft"))
	for i := 1; i <= maxPendingDeliveries; i++ {
		s.queue().Append(notice(fmt.Sprintf("n%d", i), fmt.Sprintf("notice-%d", i)))
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
	for _, it := range s2.steerQ.Items() {
		if it.Kind != steerq.User {
			sys++
			if it.Text == "notice-1" {
				hasN1 = true
			}
			continue
		}
		if it.Text == "user draft" {
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
	if q := s.steerQ.Items(); len(q) != 1 || q[0].Text != "fix the tests" || q[0].Kind != steerq.User {
		t.Fatalf("queue = %v, want one user item [fix the tests]", s.steerQ)
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
	if q := s3.steerQ.Items(); len(q) != 2 || q[0].Kind != steerq.User || q[1].Kind == steerq.User {
		t.Fatalf("queue order = %v, want [user, system]", q)
	}
	// Draining pops strictly in arrival order (user first, then system).
	m3.streaming = false
	if cmd := m3.drainSessionQueue(s3); cmd == nil {
		t.Fatal("drain must start the queued turn")
	}
	if q := s3.steerQ.Items(); len(q) != 1 || q[0].Text != "job notice" {
		t.Fatalf("after first drain queue = %v, want [job notice]", q)
	}
}

// TestSubmitQueuesWhileCancelledTurnUnwinds pins the cancel → immediate
// resend race: handleCancelKey clears the mirror (m.streaming) at once so the
// UI cannot stick on a dead stream, but the cancelled turn's goroutine keeps
// appending to the agent until its terminal message lands. A submit in that
// window must QUEUE through the steering path — starting a turn there runs two
// goroutines against one *agent.Agent and interleaves the persisted history.
func TestSubmitQueuesWhileCancelledTurnUnwinds(t *testing.T) {
	m := newQueueTestModel()
	s := m.focusedSession()
	s.streaming = true // cancelled turn still unwinding
	m.streaming = false
	seqBefore := s.turnSeq
	m.textarea.SetValue("try again")

	model, _, _ := m.handleSubmitKey(keyPress("enter"))
	m2 := model.(*Model)
	if s.steerQ.Len() != 1 || s.steerQ.Items()[0].Text != "try again" {
		t.Fatalf("queue = %v, want one user item [try again]", s.steerQ)
	}
	if s.turnSeq != seqBefore {
		t.Fatalf("a second turn started (turnSeq %d → %d)", seqBefore, s.turnSeq)
	}
	if !strings.Contains(m2.statusMsg, "Queued (1)") {
		t.Fatalf("statusMsg = %q, want queue acknowledgment", m2.statusMsg)
	}

	// The queued item must not be stranded: the cancelled turn's terminal is
	// the drain boundary (turnSeq still matches, since nothing started a new
	// turn), and it starts the queued prompt.
	seq := s.turnSeq
	model, _ = m2.handleTurnFinishedMsg(s.id, seq, context.Canceled)
	m3 := model.(*Model)
	if s.steerQ.Len() != 0 {
		t.Fatalf("queue not drained at the cancelled turn's terminal: %v", s.steerQ)
	}
	if s.turnSeq == seq {
		t.Fatal("queued prompt did not start a turn at the terminal boundary")
	}
	_ = m3
}

// TestStartTurnRefusesStreamingSession pins the safety net behind the submit
// gate: the shared turn entry point refuses a session that is still streaming
// instead of bumping its turn generation and racing the unwinding turn.
func TestStartTurnRefusesStreamingSession(t *testing.T) {
	m := newQueueTestModel()
	s := m.focusedSession()
	s.streaming = true
	seqBefore := s.turnSeq
	if cmd := m.startTurnOnWithResult(s, "hi", nil); cmd != nil {
		t.Fatal("must refuse to start a turn on a streaming session")
	}
	if s.turnSeq != seqBefore {
		t.Fatalf("turnSeq changed (%d → %d)", seqBefore, s.turnSeq)
	}
	if !strings.Contains(m.statusMsg, "Busy") {
		t.Fatalf("statusMsg = %q, want a busy notice", m.statusMsg)
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
		s.queue().Append(userItem(fmt.Sprintf("u%d", i), fmt.Sprintf("msg-%d", i)))
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
	s.steerQ = testQueue(
		userItem("u1", "queued user 1"),
		notice("s1", "queued delivery"),
		userItem("u2", "queued user 2"),
	)

	model, _, _ := m.handleCancelKey()
	m2 := model.(*Model)
	s2 := m2.focusedSession()
	if q := s2.steerQ.Items(); len(q) != 1 || q[0].Text != "queued delivery" {
		t.Fatalf("queue after cancel = %v, want [queued delivery]", q)
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
	s.steerQ = testQueue(
		userItem("u1", "first queued message"),
		notice("s1", "a system delivery"),
		userItem("u2", "third queued message"),
	)
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
	if q := s2.steerQ.Items(); len(q) != 2 || q[0].Text != "a system delivery" {
		t.Fatalf("queue after d = %v", q)
	}
	// `d` on the SYSTEM row is refused (machine bookkeeping).
	m2.queueCursor = 0
	model, _ = m2.handleQueueKey(keyPress("d"))
	m3 := model.(*Model)
	if m3.focusedSession().steerQ.Len() != 2 {
		t.Fatal("system delivery must not be deletable")
	}
	// `D` clears every user item, keeps the system one.
	model, _ = m3.handleQueueKey(keyPress("D"))
	m4 := model.(*Model)
	s4 := m4.focusedSession()
	if q := s4.steerQ.Items(); len(q) != 1 || q[0].Kind == steerq.User {
		t.Fatalf("queue after D = %v, want [system]", q)
	}
	// `s` while nothing is running does not drop the queue. The queue here
	// was already cleared by D, so this only pins the idle status.
	model, _ = m4.handleQueueKey(keyPress("s"))
	if model.(*Model).statusMsg != "The current turn has already finished." {
		t.Fatalf("idle send-now status = %q", model.(*Model).statusMsg)
	}
	// esc closes the modal.
	model, _ = m4.handleQueueKey(keyPress("esc"))
	if model.(*Model).modal != ModalNone {
		t.Fatal("esc must close the queue modal")
	}
}

// startRunningTurn starts a real turn on a fresh agent and returns once that
// turn is inside the provider. finish lets the turn answer and waits for it.
func startRunningTurn(t *testing.T) (a *agent.Agent, finish func()) {
	t.Helper()
	entered := make(chan struct{})
	release := make(chan struct{})
	p := llmtest.NewMockProvider()
	p.OnStream = func(context.Context, []llm.Message, *llm.StreamHandlers) (*llm.StreamResult, error) {
		close(entered)
		<-release
		return &llm.StreamResult{Content: "done"}, nil
	}
	a = agent.NewAgent(p, agent.NewExecutor(t.TempDir()), nil)
	done := make(chan error, 1)
	go func() {
		_, err := a.StreamProcessInput(context.Background(), "run", nil)
		done <- err
	}()
	select {
	case <-entered:
	case err := <-done:
		t.Fatalf("turn ended before the provider call: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("turn never reached the provider")
	}
	return a, func() {
		close(release)
		<-done
	}
}

// TestQueueSendNow pins the modal's send-now key against a turn that is
// really running: it asks that turn to yield only while a user message is
// queued, and it never removes queued text (a hard cancel does that).
func TestQueueSendNow(t *testing.T) {
	a, finish := startRunningTurn(t)
	defer finish()
	for _, tc := range []struct {
		name  string
		queue *steerq.Queue
		want  string
	}{
		{"nothing queued", testQueue(), "No queued message to send."},
		{"only a delivery queued", testQueue(notice("n1", "job done")), "No queued message to send."},
		{"a user message queued", testQueue(userItem("u1", "first queued message")), "Queued messages send after this round."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := newQueueTestModelFor(a)
			s := m.focusedSession()
			s.steerQ = tc.queue
			s.streaming = true
			m.modal = ModalQueue
			queued := tc.queue.Len()

			model, _ := m.handleQueueKey(keyPress("s"))
			m2 := model.(*Model)
			if m2.statusMsg != tc.want {
				t.Fatalf("status = %q, want %q", m2.statusMsg, tc.want)
			}
			if m2.focusedSession().steerQ.Len() != queued {
				t.Fatal("send now must keep the queue")
			}
			if !strings.Contains(m2.renderQueueModal(), "s send now") {
				t.Fatal("queue modal hint missing send now")
			}
		})
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
	s.steerQ = testQueue(
		userItem("u1", "first"),
		userItem("u2", "second"),
	)
	m.modal = ModalQueue
	m.queueCursor = 1 // points at "second"

	// The turn boundary's drain pops the head while the modal is open.
	s.steerQ.Take(1)

	// `d` with the stale cursor must clamp, then delete the item under the
	// clamped cursor — not panic.
	model, _ := m.handleQueueKey(keyPress("d"))
	m2 := model.(*Model)
	if got := m2.focusedSession().steerQ; got.Len() != 0 {
		t.Fatalf("queue after stale-cursor d = %v, want empty", got.Items())
	}
	if m2.queueCursor != 0 {
		t.Fatalf("cursor after emptying = %d, want 0", m2.queueCursor)
	}

	// The clamp also covers a tail shrink: the cursor re-points at the last
	// surviving row instead of highlighting nothing.
	m3 := newQueueTestModel()
	s3 := m3.focusedSession()
	s3.steerQ = testQueue(
		userItem("u1", "first"),
		userItem("u2", "second"),
		userItem("u3", "third"),
	)
	m3.modal = ModalQueue
	m3.queueCursor = 2
	s3.steerQ = testQueue( // "third" left the queue another way
		userItem("u1", "first"),
		userItem("u2", "second"),
	)
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
	s.steerQ = testQueue(userItem("u1", "one"), userItem("u2", "two"))

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
	s.steerQ = nil
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
	m2.focusedSession().steerQ = testQueue(userItem("u1", "next"))
	m2.finishFocusedTurn(nil)
	if !m2.streaming {
		t.Fatal("the drained item must have started its turn")
	}
	if m2.textarea.Placeholder != queuedComposerPlaceholder {
		t.Fatalf("placeholder with a drained turn = %q, want %q", m2.textarea.Placeholder, queuedComposerPlaceholder)
	}
}

// TestClearUserSteerKeepsMachineItems pins the interrupt semantic on the TUI
// path: /cancel drops the user's own queued text and keeps machine deliveries.
func TestClearedSteerItemsAreZeroed(t *testing.T) {
	m := newQueueTestModel()
	s := m.focusedSession()
	s.steerQ = testQueue(
		userItem("u1", "drop me"),
		notice("s1", "keep me"),
		userItem("u2", "drop me too"),
	)

	if removed := s.clearUserSteer(); removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	if q := s.steerQ.Items(); len(q) != 1 || q[0].ID != "s1" {
		t.Fatalf("queue = %+v, want the system item only", q)
	}
	// The dropped slots themselves are zeroed by the shared queue
	// (internal/steerq's TestTakeZeroesPoppedSlots and the DropKind path), so
	// the item's text and images cannot stay reachable through the backing
	// array.
}
