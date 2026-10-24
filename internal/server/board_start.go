package server

import (
	"context"
	"fmt"
	"log"
	"strings"

	"gogen/internal/agent"
)

// ticketStartOptions carries the shared start parameters for a headless
// board-ticket session (the "Start agent" button and the auto review
// agent — both seed a fresh registered session and start its first turn).
type ticketStartOptions struct {
	// label is the session's sidebar title and the board actor identity
	// (the claim/assignee text for worker starts).
	label string
	// prompt is the fully rendered seed prompt (the first turn's user
	// message).
	prompt string
	// model selects the fresh session's model ("" = the workspace default
	// seed).
	model string
	// thinkingLevel is applied after the model selection ("" = inherit the
	// seeded level); validated against the FINAL model.
	thinkingLevel string
	// seedThinkingLevel is the pane-inherited level seeded into the
	// snapshot ("" = the workspace default). Worker starts inherit the
	// active pane's live level; review sessions have no pane.
	seedThinkingLevel string
	// claim assigns the ticket to the session and moves it to in_progress
	// (worker start). Review starts never claim: the worker holds the
	// assignment, and a claim would yank the ticket out of in_review.
	claim bool
	// owner is the initiating connection: it passively receives the
	// headless turn's delete approvals and owns the turn (interrupt
	// semantics). nil (auto review start) attaches nothing — the
	// deny-when-unattended approval override covers the zero-client case.
	owner *wsConn
}

// startTicketSession creates, registers, and starts the headless agent
// session for a board ticket. Shared by the web "Start agent" button and
// the auto review trigger so the two cannot drift (model selection order,
// level validation, approval wiring, failure unwind). Returns the runtime
// and session id. On error everything is unwound: the fresh session is
// removed and no claim survives (a claimed ticket is reset to backlog).
// The caller surfaces the error on its own channel — the WS start op maps
// it to a board notice, the review trigger logs it and comments the
// ticket.
func (s *Server) startTicketSession(ctx context.Context, item *agent.BoardItem, opts ticketStartOptions) (*sessionRuntime, string, error) {
	if s.ws.Store == nil {
		return nil, "", fmt.Errorf("session persistence is disabled")
	}
	// Refuse before creating anything when neither the options nor the
	// workspace provides a model: the fresh session's provider seeds from
	// the workspace default, and a first turn without a model would fail
	// silently (nobody is attached). An explicit model skips the default
	// entirely.
	if strings.TrimSpace(opts.model) == "" && s.ws.DefaultModel() == "" {
		return nil, "", fmt.Errorf("no model configured — pick a model in the start dialog or set one in Settings first")
	}
	bm := s.ws.GetBoardManager()
	snap := &agent.SessionSnapshot{
		WorkingDir: s.ws.GetWorkingDir(),
		// Messages deliberately NOT seeded: startTurn passes opts.prompt
		// as the turn's user message, so the transcript would otherwise
		// carry the prompt twice.
	}
	if opts.seedThinkingLevel != "" {
		snap.ThinkingLevel = opts.seedThinkingLevel
	}
	rt, _ := s.registerSeededSession(snap, opts.label)
	sid := rt.agent.SessionID

	// Model selection BEFORE any board mutation so a selection failure
	// unwinds with only the fresh-session cleanup (no claim to reset).
	// Catalog lookups are network I/O — done outside any turn lock, same
	// as set_model.
	if strings.TrimSpace(opts.model) != "" {
		if err := rt.agent.SelectModel(ctx, strings.TrimSpace(opts.model)); err != nil {
			s.removeFreshSession(sid)
			return nil, sid, err
		}
	}

	// Reasoning effort: validated against the FINAL model (after
	// SelectModel above, mirroring the subagent effort cascade) and
	// applied before any board mutation so a failure unwinds with only
	// the fresh-session cleanup. Empty keeps the seed level; "off" is a
	// real state (never send reasoning_effort) that overrides the
	// inheritance.
	if strings.TrimSpace(opts.thinkingLevel) != "" {
		level := string(agent.NormalizeThinkingLevel(opts.thinkingLevel))
		if level == "" || !s.isValidThinkingLevel(rt.agent, opts.thinkingLevel) {
			s.removeFreshSession(sid)
			return nil, sid,
				fmt.Errorf("reasoning-effort level %q is not accepted by the selected model", strings.TrimSpace(opts.thinkingLevel))
		}
		rt.agent.SetThinkingLevel(agent.ThinkingLevel(level))
	}

	if opts.claim {
		if _, err := bm.Claim(item.ID, opts.label); err != nil {
			// A concurrent start won the claim: remove the fresh session
			// and report the claim error — no orphan empty sessions.
			s.removeFreshSession(sid)
			return nil, sid, err
		}
		if _, err := bm.AttachAgent(item.ID, sid, opts.label); err != nil {
			// The claim persisted but the link write failed (disk error):
			// the session is still valid — log and continue; the ticket
			// keeps its assignee and the session is reachable from the
			// sidebar.
			log.Printf("board start: attach agent link on #%s: %v", item.ID, err)
		}
	} else if _, err := bm.AttachReviewAgent(item.ID, sid, opts.label); err != nil {
		// Non-fatal: the review session still runs; the ticket lacks the
		// review link (and the rounds bump) until the next move. A stale
		// link is harmless — the duplicate guard checks registry
		// liveness, not the stored id.
		log.Printf("board start: attach review link on #%s: %v", item.ID, err)
	}

	// Unattended approval guard: with zero attached clients a delete
	// approval can never be answered, so deny it fail-closed (the agent
	// adapts to the denied result). Once the user opens the session, the
	// normal approval flow applies.
	rt.approverOverride = func(ctx context.Context, req agent.DeleteRequest) (bool, error) {
		if rt.clientCount() == 0 {
			return false, nil
		}
		return rt.deleteApprover()(ctx, req)
	}
	// A blocked command needs no override here: commandApprover already denies
	// fail-closed at zero attached clients (deleteApprover instead parks the
	// request, which is why the wrapper above exists for deletes only).
	// First-turn failure visibility: comment the error on the ticket.
	ticketID := item.ID
	rt.turnErrorHook = func(err error) {
		if _, cerr := bm.Comment(ticketID, fmt.Sprintf("agent session %s failed: %v", sid, err), "system"); cerr != nil {
			log.Printf("board start: comment turn error on #%s: %v", ticketID, cerr)
		}
	}

	// Acquire the turn BEFORE attaching the tab: a failed acquire (runtime
	// evicted / busy) must not leave a tab attached to a session it was
	// never told about, and the cleanup below has nothing to unwind. A
	// worker start additionally resets the ticket (a failed acquire would
	// otherwise strand a claimed-but-idle ticket); a review start needs no
	// cleanup — its stale review link does not block a later review.
	if !rt.acquireTurnForHandler(opts.owner) {
		s.removeFreshSession(sid)
		if opts.claim {
			if _, err := bm.ResetAgent(item.ID, "user"); err != nil {
				log.Printf("board start: reset ticket #%s after failed start: %v", item.ID, err)
			}
		}
		return nil, sid, fmt.Errorf("could not start the agent session for board item #%s; the ticket was reset", item.ID)
	}

	// Background attach (worker start): the initiating tab becomes an
	// APPROVAL RECEIVER of the ticket session WITHOUT viewing it
	// (attachPassive, not attach) and without switching the pane or main
	// tab (the user stays on the board). Delete approvals reach the tab —
	// the client renders the approval modal globally, even though no pane
	// owns the session — and closing the tab detaches normally, so the
	// standard approval-hold / auto-deny-on-detach machinery applies
	// (clientCount includes passive attachments). The passive role keeps
	// the session OUT of the live-session signal (viewerCount): once the
	// turn ends, the orphan eviction releases the runtime, so the sidebar
	// never pins a stale "resume to continue" row for a completed ticket
	// nobody is viewing. The deny-when-unattended override above still
	// covers approvals that arrive after the tab is gone (between detach
	// and turn end). Review starts have no owner connection: the session
	// runs with zero clients and evicts the same way.
	if opts.owner != nil && !rt.attachPassive(opts.owner) {
		// Unreachable in practice: rt was registered moments above (a fresh
		// runtime nobody can have claimed or evicted yet). If an eviction
		// somehow won the race, the start still proceeds headless — with no
		// approval receiver the deny-when-unattended override above applies,
		// exactly as for a review start (which has no owner at all).
		log.Printf("board start: owner attach refused for #%s (runtime evicting)", item.ID)
	}
	rt.startTurn(opts.owner, opts.prompt, nil)
	// No board_state broadcast here: the caller finishes its ticket
	// bookkeeping first (the worker start persists the popover's options;
	// broadcasting before that would show a stale card), then broadcasts.
	return rt, sid, nil
}

// wsHandleBoardStart starts a dedicated agent session for a board ticket
// (board_op action "start"): the ticket is claimed (assignee = the session
// label), a fresh session is seeded with the ticket prompt and registered
// headless — the user's pane and main tab are untouched — and its first
// turn starts immediately. The session id is stored on the ticket
// (AgentSessionID) so the client can offer "Open agent" on a second click.
//
// Unattended-session guards: a delete approval with zero attached clients
// can never be answered (auto-deny only fires on detach), so the runtime
// denies them fail-closed until the user opens the session; a first-turn
// failure is commented on the ticket via the turnErrorHook so a claimed
// ticket never fails silently. Failures surface as board error notices;
// a lost claim race cleans up the fresh session (no orphan empty sessions).
//
// The op may carry per-ticket start options: Model ("" = workspace default)
// and Prompt ("" = the configured board_start_prompt template; the ticket's
// stored override is cleared). The model is selected BEFORE the claim so a
// selection failure unwinds with just the fresh-session cleanup; the
// options are persisted on the ticket after the claim so the start popover
// pre-fills on the next start.
func (s *Server) wsHandleBoardStart(ctx context.Context, ws *wsConn, pane **sessionRuntime, op *BoardOpRequest) {
	if s.ws.Store == nil {
		writeNoticeError(ws, "board", "Error: session persistence is disabled")
		return
	}
	bm := s.ws.GetBoardManager()
	item, err := bm.Item(op.ID)
	if err != nil {
		writeNoticeError(ws, "board", "Error: "+err.Error())
		return
	}
	if item.Status == "done" {
		writeNoticeError(ws, "board", fmt.Sprintf("Error: board item #%s is done — move it back to start it again", item.ID))
		return
	}
	// The previous agent session for this ticket was deleted: reset the
	// stale link so the ticket can be started fresh.
	if item.AgentSessionID != "" && !s.sessionExists(item.AgentSessionID) {
		if _, err := bm.ResetAgent(item.ID, "user"); err != nil {
			writeNoticeError(ws, "board", "Error: "+err.Error())
			return
		}
		if item, err = bm.Item(item.ID); err != nil {
			writeNoticeError(ws, "board", "Error: "+err.Error())
			return
		}
	}
	// Mirror createNewSession: the seeded snapshot inherits the pane's
	// thinking level ("" keeps the workspace default seed).
	var seedLevel string
	if old := *pane; old != nil && old.agent != nil {
		if _, level := old.agent.ModeAndThinkingLevel(); level != "" {
			seedLevel = string(level)
		}
	}
	label := ticketSessionLabel(item)
	prompt := agent.TicketPrompt(item, s.ws.GetRuntimeConfig().BoardStartPrompt)
	// Per-ticket prompt template from the start popover wins over the
	// configured template (TicketPrompt substitutes the placeholders).
	if strings.TrimSpace(op.Prompt) != "" {
		prompt = agent.TicketPrompt(item, op.Prompt)
	}
	_, sid, err := s.startTicketSession(ctx, item, ticketStartOptions{
		label:             label,
		prompt:            prompt,
		model:             op.Model,
		thinkingLevel:     op.ThinkingLevel,
		seedThinkingLevel: seedLevel,
		claim:             true,
		owner:             ws,
	})
	if err != nil {
		writeNoticeError(ws, "board", "Error: "+err.Error())
		return
	}
	// Persist the start options (per-ticket model/prompt prefill for the
	// next start). The start op is authoritative: an empty value clears
	// the stored override back to the defaults. Non-fatal — the session
	// already started; the popover just re-falls back to the defaults.
	if err := bm.SetStartOptions(item.ID, op.Model, op.Prompt, op.ThinkingLevel); err != nil {
		log.Printf("board start: persist start options on #%s: %v", item.ID, err)
	}
	// Per-ticket review options ride the start op (the card popover's
	// review section): same authoritative-empty-clears contract, same
	// prefill guarantee (the popover pre-fills from the stored values, so
	// an untouched section re-sends them). Non-fatal for the same reason.
	if _, err := bm.SetReviewOptions(item.ID, op.ReviewModel, op.ReviewThinkingLevel, "user"); err != nil {
		log.Printf("board start: persist review options on #%s: %v", item.ID, err)
	}
	s.broadcastBoardState()
	writeNotice(ws, "board", true, fmt.Sprintf("Started agent session %s for board item #%s: %s", sid, item.ID, item.Title))
}

// ticketSessionLabel derives the started session's sidebar label and board
// assignee from the ticket (same text, so the agent's own board actions use
// the identity the ticket was claimed with).
func ticketSessionLabel(item *agent.BoardItem) string {
	label := fmt.Sprintf("ticket #%s: %s", item.ID, item.Title)
	if len(label) > 60 {
		label = label[:60] + "…"
	}
	return label
}

// sessionExists reports whether a session id is live (registry) or saved
// (store). Used to detect stale ticket→session links.
func (s *Server) sessionExists(id string) bool {
	if id == "" {
		return false
	}
	if _, ok := s.registry.get(id); ok {
		return true
	}
	if s.ws.Store == nil {
		return false
	}
	_, err := s.ws.Store.LoadInWorkingDir(s.ws.GetWorkingDir(), id)
	return err == nil
}

// removeFreshSession evicts a just-created session that must not survive
// (the board start's claim lost a race): delete the file and unregister the
// runtime. No clients are attached and no turn has started.
func (s *Server) removeFreshSession(id string) {
	if s.ws.Store != nil {
		if err := s.ws.Store.Delete(s.ws.GetWorkingDir(), id); err != nil {
			log.Printf("board start: delete fresh session %s: %v", id, err)
		}
	}
	s.registry.remove(id)
}
