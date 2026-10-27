package server

import (
	"fmt"

	"gogen/internal/agent"
)

// wsHandleBoardOp handles kanban-tab operations ("board_op"): the client
// sends list (no mutation) or add/update/claim/move/block/comment/done/
// start/remove; successful mutations broadcast a fresh board_state to every
// client so all tabs stay live and toast a success notice to the requesting
// connection (every mutation gets visible feedback — including
// agent-triggered ones, which broadcast through the on-board-changed hook
// instead). Feedback never goes out as "response" (which renders into the
// chat transcript). The handler is gated on the board feature flag — a stale
// client's op after a toggle-off is rejected with an error notice.
func wsHandleBoardOp(req *wsRequest) {
	s, ws, r, msg := req.server, req.conn, req.request, req.msg
	pane := &req.pane
	if !s.ws.GetBoardEnabled() {
		writeNoticeError(ws, "board", "Error: the board feature is disabled")
		return
	}
	bm := s.ws.GetBoardManager()
	if bm == nil {
		writeNoticeError(ws, "board", "Error: board manager is not initialized")
		return
	}
	op := msg.BoardOp
	if op == nil {
		writeNoticeError(ws, "board", "Error: boardOp is required")
		return
	}
	if op.Action == "list" {
		snap, err := bm.Snapshot()
		if err != nil {
			writeNoticeError(ws, "board", "Error: "+err.Error())
			return
		}
		_ = ws.writeJSON(WSMessage{Type: "board_state", BoardState: snap})
		return
	}
	const by = "user"
	var out string
	var err error
	switch op.Action {
	case "add":
		out, err = bm.Add(op.Title, op.Description, op.Priority, by)
	case "update":
		// The edit form always submits all three editable fields (a full
		// replace); empty priority/description clear their fields.
		out, err = bm.Update(op.ID, agent.BoardUpdate{
			Title:       &op.Title,
			Description: &op.Description,
			Priority:    &op.Priority,
			Context:     &op.Context,
		}, by)
	case "claim":
		out, err = bm.Claim(op.ID, by)
	case "move":
		out, err = bm.Move(op.ID, op.Column, by)
	case "block":
		out, err = bm.Block(op.ID, op.Reason, by)
	case "comment":
		out, err = bm.Comment(op.ID, op.Text, by)
	case "done":
		out, err = bm.Done(op.ID, by)
	case "remove":
		out, err = bm.Delete(op.ID)
	case "start":
		// Start a dedicated agent session for the ticket (headless; the
		// user stays on the board). Handles its own board_state broadcast
		// and notice.
		s.wsHandleBoardStart(r.Context(), ws, pane, op)
		return
	default:
		err = fmt.Errorf("unknown board op %q (want list, add, update, claim, move, block, comment, done, start, or remove)", op.Action)
	}
	if err != nil {
		writeNoticeError(ws, "board", "Error: "+err.Error())
		return
	}
	s.broadcastBoardState()
	// Every successful mutation toasts its confirmation to the initiator.
	writeNotice(ws, "board", true, out)
}

// broadcastBoardState pushes the current board snapshot to every attached
// client of every live session (the kanban tab re-renders from it). Called
// after board mutations from the web UI (wsHandleBoardOp) and from agent
// board-tool calls via the agent's on-board-changed hook, so a board the
// user is watching stays live while agents work on it.
func (s *Server) broadcastBoardState() {
	bm := s.ws.GetBoardManager()
	if bm == nil {
		return
	}
	snap, err := bm.Snapshot()
	if err != nil {
		return
	}
	// broadcastAll, not broadcast: the board is process-wide and a socket
	// attached to several panes must receive ONE frame (see registry.broadcastAll).
	s.registry.broadcastAll(WSMessage{Type: "board_state", BoardState: snap})
}

// broadcastBoardNotice toasts a board mutation to every attached client of
// every live session. Used for AGENT-triggered mutations (via the
// on-board-changed hook): the user sees "Moved board item #3 to
// in_progress…" even when a running agent made the change. Web-UI ops toast
// only the requesting connection (writeNotice in wsHandleBoardOp) — the
// initiator already knows what they did.
func (s *Server) broadcastBoardNotice(msg string) {
	if msg == "" {
		return
	}
	s.registry.broadcastAll(WSMessage{Type: "notice", Kind: "board", Success: true, Content: msg})
}
