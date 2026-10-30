package server

// writeNotice sends a non-chat UI-feedback message ("notice") to a client.
// The client toasts it and never renders it into the chat transcript, so
// UI-channel handlers (board ops, settings toggles, working-dir input, model
// picker, sidebar refreshes) report errors here — see the message-type
// contract in ws_types.go. kind scopes the notice for client-side follow-ups
// ("board", "settings", "workspace", "model", "sessions", "models", …).
func writeNotice(ws *wsConn, kind string, success bool, content string) {
	if ws == nil {
		return
	}
	_ = ws.writeJSON(WSMessage{Type: "notice", Kind: kind, Success: success, Content: content})
}

// writeNoticeError is writeNotice for the common error case.
func writeNoticeError(ws *wsConn, kind, errMsg string) {
	writeNotice(ws, kind, false, errMsg)
}

// writeNoticeFor is writeNotice tagged with the session it concerns, for the
// kinds whose client-side follow-up is per-pane. A refused "queue" notice is
// the case: it must take back the pending Send now of the pane that asked,
// which is not necessarily the pane the client has focused by the time the
// answer arrives.
func writeNoticeFor(ws *wsConn, sessionID, kind string, success bool, content string) {
	if ws == nil {
		return
	}
	_ = ws.writeJSON(WSMessage{Type: "notice", SessionID: sessionID, Kind: kind, Success: success, Content: content})
}

// writeNoticeErrorFor is writeNoticeFor for the common error case.
func writeNoticeErrorFor(ws *wsConn, sessionID, kind, errMsg string) {
	writeNoticeFor(ws, sessionID, kind, false, errMsg)
}
