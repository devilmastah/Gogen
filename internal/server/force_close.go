package server

import (
	"net/http"
)

func (s *Server) trackHTTPServer(srv *http.Server) {
	s.httpSrv.Store(srv)
}

func (s *Server) untrackHTTPServer() {
	s.httpSrv.Store(nil)
}

// ForceClose closes tracked WebSockets and the HTTP listener immediately so
// Start can return on Ctrl+C / ctx cancel. Shutdown alone does not close
// hijacked WebSocket connections (see net/http.Server.Shutdown docs).
//
// It then joins the WebSocket handler goroutines (bounded): net/http stops
// tracking a connection the moment the upgrade hijacks it, so http.Server.Close
// — and httptest.Server.Close in tests — returns while a read loop may still be
// dispatching its last message. Such a handler detaches sessions, and a detach
// can orphan-evict a runtime, whose eviction flushes the session store. Waiting
// here makes the caller's shutdown flush (ShutdownSessions) the LAST writer
// instead of racing it, which is what the flush's recency contract assumes.
func (s *Server) ForceClose() {
	if s != nil {
		s.closeWSConns()
	}
	if srv := s.httpSrv.Load(); srv != nil {
		_ = srv.Close()
	}
	if s != nil {
		s.WaitWSHandlers(wsHandlerDrainWait)
	}
}
