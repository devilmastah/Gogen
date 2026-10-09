package server

import (
	"testing"
	"time"
)

// TestBroadcastStatusDisplayRelaysFrames pins browser mode's relay: the
// server builds the row (statusdisplay sink) and broadcasts it to every
// attached client as a `status_display` frame, which the browser turns into
// the HTTP call. The clear frame carries the row name so the browser can
// DELETE it.
func TestBroadcastStatusDisplayRelaysFrames(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	s.broadcastStatusDisplay([]byte(`{"source":"gogen-ttgo","state":"running"}`), false, "gogen-ttgo")
	row := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "status_display" })
	if row.StatusDisplayClear {
		t.Fatal("row frame marked as clear")
	}
	if row.StatusDisplayRow != `{"source":"gogen-ttgo","state":"running"}` {
		t.Fatalf("row = %q, want the built JSON verbatim", row.StatusDisplayRow)
	}
	if row.StatusDisplaySource != "gogen-ttgo" {
		t.Fatalf("source = %q, want gogen-ttgo", row.StatusDisplaySource)
	}

	s.broadcastStatusDisplay(nil, true, "gogen-ttgo")
	clear := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "status_display" })
	if !clear.StatusDisplayClear {
		t.Fatal("clear frame not marked as clear")
	}
	if clear.StatusDisplayRow != "" {
		t.Fatalf("clear row = %q, want empty", clear.StatusDisplayRow)
	}
	if clear.StatusDisplaySource != "gogen-ttgo" {
		t.Fatalf("clear source = %q, want gogen-ttgo", clear.StatusDisplaySource)
	}
}
