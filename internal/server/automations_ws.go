package server

import (
	"fmt"
	"time"

	"gogen/internal/automation"
)

// The automations tab's WS surface (mirrors the board tab's contract):
// the client sends "automations_op" messages (list/runs reads, create/
// enable/disable/delete mutations); reads reply only to the requester,
// mutations broadcast a fresh "automations_state" to every client and toast
// a success notice to the initiator. Store I/O runs off the WS read loop
// (the store is a JSON file, unlike the board manager's in-memory state),
// sharing the SAME store instance the scheduler sweeps — see
// automationStore for why one instance is load-bearing.
func init() {
	wsHandlers["automations_op"] = wsHandlerEntry{handle: wsHandleAutomationsOp}
}

// wsHandleAutomationsOp validates the envelope and moves the (disk-backed)
// store work off the WS read loop, like the sessions listing.
func wsHandleAutomationsOp(req *wsRequest) {
	s, ws, msg := req.server, req.conn, req.msg
	if !s.ws.GetAutomationsEnabled() {
		writeNoticeError(ws, "automations", "Error: the automations feature is disabled (Settings → Agent)")
		return
	}
	op := msg.AutomationsOp
	if op == nil {
		writeNoticeError(ws, "automations", "Error: automationsOp is required")
		return
	}
	go s.handleAutomationsOp(ws, op)
}

// handleAutomationsOp performs one op. All failures surface as error
// notices on the "automations" kind (toasts; never the chat transcript).
func (s *Server) handleAutomationsOp(ws *wsConn, op *AutomationsOpRequest) {
	switch op.Action {
	case "list":
		_ = ws.writeJSON(WSMessage{Type: "automations_state", AutomationsState: s.automationsSnapshot()})
	case "runs":
		s.automationsRunsOp(ws, op)
	case "create":
		s.automationsCreateOp(ws, op)
	case "update":
		s.automationsUpdateOp(ws, op)
	case "enable", "disable":
		s.automationsSetEnabledOp(ws, op, op.Action == "enable")
	case "delete":
		s.automationsDeleteOp(ws, op)
	default:
		writeNoticeError(ws, "automations",
			fmt.Sprintf("Error: unknown automations op %q (want list, runs, create, update, enable, disable, or delete)", op.Action))
	}
}

// automationsSnapshot builds the tab payload. A store failure degrades the
// snapshot (StoreOK=false + error text) instead of dropping the reply, so
// the tab can render the problem instead of spinning on a missing push.
func (s *Server) automationsSnapshot() *AutomationsState {
	state := &AutomationsState{
		Enabled:     s.ws.GetAutomationsEnabled(),
		Automations: []automation.Automation{},
	}
	store, err := s.automationStore()
	if err != nil {
		state.StoreError = err.Error()
		return state
	}
	state.StoreOK = true
	state.Automations = store.List()
	return state
}

func (s *Server) automationsRunsOp(ws *wsConn, op *AutomationsOpRequest) {
	store, err := s.automationStore()
	if err != nil {
		writeNoticeError(ws, "automations", "Error: "+err.Error())
		return
	}
	if _, ok := store.Get(op.ID); !ok {
		writeNoticeError(ws, "automations", fmt.Sprintf("Error: no automation with id %s", op.ID))
		return
	}
	runs := store.Runs(op.ID)
	_ = ws.writeJSON(WSMessage{Type: "automations_runs", AutomationsRuns: &AutomationsRunsPayload{
		AutomationID: op.ID,
		Runs:         runs,
	}})
}

func (s *Server) automationsCreateOp(ws *wsConn, op *AutomationsOpRequest) {
	a := automation.Automation{
		Title:      op.Title,
		Prompt:     op.Prompt,
		WorkingDir: op.WorkingDir,
		Schedule: automation.Schedule{
			Kind:     op.Kind,
			At:       op.At,
			Time:     op.Time,
			Minute:   op.Minute,
			Weekdays: op.Weekdays,
			Timezone: op.Timezone,
		},
		Enabled: !op.StartDisabled,
	}
	// The workspace working dir is the default, like the CLI's cwd.
	if a.WorkingDir == "" {
		a.WorkingDir = s.ws.GetWorkingDir()
	}
	store, err := s.automationStore()
	if err != nil {
		writeNoticeError(ws, "automations", "Error: "+err.Error())
		return
	}
	if err := store.Create(&a); err != nil {
		writeNoticeError(ws, "automations", "Error: "+err.Error())
		return
	}
	s.broadcastAutomationsState()
	writeNotice(ws, "automations", true, fmt.Sprintf("Created automation %s %q (next run %s)",
		a.ID, a.Title, formatAutomationsTime(a.NextRunAt)))
}

// automationsUpdateOp applies a PATCH-style edit from the tab's edit form.
// The client sends the FULL schedule (like create) plus the editable
// fields; the store's Update decides whether next_run_at is re-timed
// (schedule change or fresh-enable) or kept (prompt-only edits). Empty
// optional fields (timezone/at/time) are passed through as empty — the
// client sends the complete schedule, so empty means "empty", not "keep".
func (s *Server) automationsUpdateOp(ws *wsConn, op *AutomationsOpRequest) {
	store, err := s.automationStore()
	if err != nil {
		writeNoticeError(ws, "automations", "Error: "+err.Error())
		return
	}
	// An omitted working dir keeps the stored one (the tab's edit form
	// always sends it; tests and thin clients may not).
	workingDir := op.WorkingDir
	if workingDir == "" {
		if stored, ok := store.Get(op.ID); ok {
			workingDir = stored.WorkingDir
		}
	}
	updated := automation.Automation{
		ID:         op.ID,
		Title:      op.Title,
		Prompt:     op.Prompt,
		WorkingDir: workingDir,
		Schedule: automation.Schedule{
			Kind:     op.Kind,
			At:       op.At,
			Time:     op.Time,
			Minute:   op.Minute,
			Weekdays: op.Weekdays,
			Timezone: op.Timezone,
		},
		Enabled: !op.StartDisabled,
	}
	if err := store.Update(&updated); err != nil {
		writeNoticeError(ws, "automations", "Error: "+err.Error())
		return
	}
	// Report the fresh record (Update may have re-timed next_run_at).
	a, _ := store.Get(op.ID)
	s.broadcastAutomationsState()
	writeNotice(ws, "automations", true, fmt.Sprintf("Updated %s %q (next run %s)",
		a.ID, a.Title, formatAutomationsTime(a.NextRunAt)))
}

func (s *Server) automationsSetEnabledOp(ws *wsConn, op *AutomationsOpRequest, on bool) {
	store, err := s.automationStore()
	if err != nil {
		writeNoticeError(ws, "automations", "Error: "+err.Error())
		return
	}
	if err := store.SetEnabled(op.ID, on); err != nil {
		writeNoticeError(ws, "automations", "Error: "+err.Error())
		return
	}
	a, _ := store.Get(op.ID)
	verb := "Enabled"
	if !on {
		verb = "Disabled"
	}
	s.broadcastAutomationsState()
	if on {
		writeNotice(ws, "automations", true, fmt.Sprintf("%s %s %q; next run %s", verb, a.ID, a.Title, formatAutomationsTime(a.NextRunAt)))
		return
	}
	writeNotice(ws, "automations", true, fmt.Sprintf("%s %s %q (paused)", verb, a.ID, a.Title))
}
func (s *Server) automationsDeleteOp(ws *wsConn, op *AutomationsOpRequest) {
	store, err := s.automationStore()
	if err != nil {
		writeNoticeError(ws, "automations", "Error: "+err.Error())
		return
	}
	a, ok := store.Get(op.ID)
	if !ok {
		writeNoticeError(ws, "automations", fmt.Sprintf("Error: no automation with id %s", op.ID))
		return
	}
	if err := store.Delete(op.ID); err != nil {
		writeNoticeError(ws, "automations", "Error: "+err.Error())
		return
	}
	s.broadcastAutomationsState()
	writeNotice(ws, "automations", true, fmt.Sprintf("Deleted automation %s %q (and its run history)", a.ID, a.Title))
}

// broadcastAutomationsState pushes the tab snapshot to every attached
// client (after every mutation, so open tabs stay in sync — mirroring
// board_state). Failures to snapshot degrade to a StoreError payload
// rather than silence.
func (s *Server) broadcastAutomationsState() {
	snap := s.automationsSnapshot()
	// Server-wide payload: broadcastAll sends it once per socket (a socket
	// attached to several panes would otherwise get one copy per pane).
	s.registry.broadcastAll(WSMessage{Type: "automations_state", AutomationsState: snap})
}

// formatAutomationsTime renders an optional timestamp for notices ("" when
// unset).
func formatAutomationsTime(t *time.Time) string {
	if t == nil {
		return "—"
	}
	return t.Local().Format("2006-01-02 15:04")
}
