package server

import (
	"gogen/internal/config"
	"gogen/internal/statusdisplay"
)

// applyStatusDisplayStartup applies the browser-mode gate when the server
// starts: the server probes for the display first (server transport wins)
// and falls back to the browser relay when it cannot reach it. The
// runtime-config applyLive path only runs on changes, and main's startup
// application deliberately skips browser mode (the TUI/CLI has no browser),
// so the web server owns this first application. Other modes are left to
// main's applyRuntimeConfig (server-side reporter / probe).
func (s *Server) applyStatusDisplayStartup() {
	if s == nil || s.ws == nil {
		return
	}
	r := s.ws.GetRuntimeConfig()
	if r.StatusDisplayMode() != config.StatusDisplayBrowser {
		return
	}
	statusdisplay.ProbeWithRelay(r.StatusDisplayHost, r.StatusDisplayToken, r.StatusDisplaySource, s.ws.GetWorkingDir(), s.broadcastStatusDisplay)
}

// broadcastStatusDisplay relays one display operation to every attached
// client of every session. The reporter is process-wide (one row,
// last-writer-wins across sessions), so the relay is not scoped to a session
// runtime.
//
// Installed as the statusdisplay sink in `status_display: browser` mode: the
// server still owns the turn state and builds every row, but the browser
// performs the actual HTTP call, because the browser may be on the display's
// network when the server is not. Runs on the reporter's worker goroutine;
// broadcasting is safe from any goroutine.
func (s *Server) broadcastStatusDisplay(row []byte, clear bool, source string) {
	msg := WSMessage{
		Type:                "status_display",
		StatusDisplayClear:  clear,
		StatusDisplaySource: source,
	}
	if !clear {
		msg.StatusDisplayRow = string(row)
	}
	for _, id := range s.registry.activeIDs() {
		if rt, ok := s.registry.get(id); ok {
			rt.broadcast(msg)
		}
	}
}
