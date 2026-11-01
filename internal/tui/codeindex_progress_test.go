package tui

import (
	"strings"
	"testing"
)

func TestCodeIndexProgressAppendsOnlyAfterEmbed(t *testing.T) {
	m := &Model{}
	m.handleMsg(codeIndexProgressMsg{line: "Scanning files", phase: "walking"})
	if m.codeIndexBusy {
		t.Fatal("a scan marked the rebuild busy")
	}
	if len(m.chatLines) != 0 {
		t.Fatalf("scan appended %v", m.chatLines)
	}
	m.handleMsg(codeIndexProgressMsg{line: "3 files, 9 chunks", phase: "idle"})
	if len(m.chatLines) != 0 {
		t.Fatalf("unchanged search appended %v", m.chatLines)
	}

	m.handleMsg(codeIndexProgressMsg{line: "Embedding 1/1 files", phase: "embedding"})
	if !m.codeIndexBusy {
		t.Fatal("embedding did not mark the rebuild busy")
	}
	m.handleMsg(codeIndexProgressMsg{line: "embed failed", phase: "idle", err: "embed failed"})
	if len(m.chatLines) != 0 {
		t.Fatalf("progress error appended %v", m.chatLines)
	}
	if m.statusMsg != "embed failed" {
		t.Fatalf("status = %q", m.statusMsg)
	}
	m.handleMsg(codeIndexDoneMsg{err: errString("embed failed")})
	if len(m.chatLines) != 1 || !strings.Contains(m.chatLines[0], "embed failed") {
		t.Fatalf("done line = %v", m.chatLines)
	}

	m = &Model{}
	m.handleMsg(codeIndexProgressMsg{line: "Embedding 1/1 files", phase: "embedding"})
	m.handleMsg(codeIndexProgressMsg{line: "1 file, 1 chunk", phase: "idle"})
	if len(m.chatLines) != 1 || !strings.Contains(m.chatLines[0], "1 file, 1 chunk") {
		t.Fatalf("success line = %v", m.chatLines)
	}
	m.handleMsg(codeIndexDoneMsg{})
	if len(m.chatLines) != 1 {
		t.Fatalf("success appended again: %v", m.chatLines)
	}

	m = &Model{}
	m.handleMsg(codeIndexProgressMsg{line: "Refresh failed: down", phase: "idle", err: "down"})
	if m.statusMsg != "Refresh failed: down" {
		t.Fatalf("watch status = %q", m.statusMsg)
	}
	if len(m.chatLines) != 0 {
		t.Fatalf("watch error appended %v", m.chatLines)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
