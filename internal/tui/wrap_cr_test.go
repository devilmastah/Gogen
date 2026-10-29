package tui

import (
	"strings"
	"testing"
	"unsafe"

	"gogen/internal/llm"

	"github.com/charmbracelet/x/ansi"
)

// Regression: a carriage return in a chat line is the one raw control byte
// that survives the whole render pipeline as a real cursor move. In
// ultraviolet's printString — the parser behind bubbletea's inline
// renderer — exactly three inputs move the pen: "\n" (y++), "\r" (x = the
// FRAME's left margin) and SGR. Every other escape is dropped, but a CR is
// honoured, so the renderer sent the pen back to column 0 and repainted over
// the sidebar, the divider and the left of the transcript row.
//
// Command output is the source. execute_command and bash_persistent return
// child bytes verbatim, and handleStreamToolResult splits a result on "\n"
// only, so CRLF output leaves a trailing CR on every line and a progress bar
// ("10%\r50%\rdone") leaves mid-line ones. The painted frame then disagreed
// with the model's cell model, which is what broke the layout. These tests
// pin that no CR reaches the viewport, on either the live or history path.

// wrapCRTestModel returns a wide-enough model that wrapping is not what is
// under test; the tests isolate CR handling from wrap-boundary behaviour.
func wrapCRTestModel() *Model {
	m := newSidebarTestModel(100)
	m.viewport.Width = 200
	m.viewport.Style = ViewportStyle
	return m
}

func TestNormalizeCR(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"no cr is returned unchanged", "plain text", "plain text"},
		{"empty string", "", ""},
		{"crlf collapses to lf", "a\r\nb", "a\nb"},
		{"trailing crlf", "a\r\n", "a\n"},
		{"leading crlf", "\r\na", "\na"},
		{"lone cr is dropped", "a\rb", "ab"},
		{"progress bar crs are dropped", "10%\r50%\rdone", "10%50%done"},
		{"trailing bare cr", "abc\r", "abc"},
		{"lone cr", "\r", ""},
		{"cr before crlf", "a\r\r\nb", "a\nb"},
		{"consecutive crs", "a\r\r\rb", "ab"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := normalizeCR(tc.in); got != tc.want {
				t.Fatalf("normalizeCR(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

// normalizeCR must not corrupt multi-byte runes: it edits bytes, and a
// dropped CR must never take a UTF-8 continuation byte with it.
func TestNormalizeCRKeepsMultibyteIntact(t *testing.T) {
	got := normalizeCR("café\r\n你好\r世界\U0001F600")
	if !strings.Contains(got, "café") || !strings.Contains(got, "你好") ||
		!strings.Contains(got, "世界\U0001F600") {
		t.Fatalf("normalizeCR damaged multi-byte content: %q", got)
	}
}

// The viewport lines are what actually get painted, so no part of a wrapped
// chat line may still hold a CR — this is the invariant that keeps the
// renderer's cell model and the transcript in agreement.
func TestWrapLineStripsCarriageReturns(t *testing.T) {
	m := wrapCRTestModel()
	cases := []struct {
		name string
		line string
	}{
		{"crlf output", "Downloading 10%\r\nDownloading 100%\r\ndone\r\n"},
		{"progress bar crs", "Downloading 10%\rDownloading 100%\rdone"},
		{"trailing cr", "output line\r"},
		{"lone cr", "\r"},
		{"cr between styled segments", "a\r\x1b[31mb\x1b[0m"},
		{"no cr control", "ordinary output line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			for i, part := range m.wrapLine(tc.line) {
				if strings.ContainsRune(part, '\r') {
					t.Fatalf("part %d still carries a CR: %q", i, part)
				}
			}
		})
	}
}

// A CR must not change how much text is shown: the fix removes the
// cursor move, not the content. Stripping the CRs and the LF split must
// yield the same visible characters.
func TestWrapLineStripsCRWithoutLosingText(t *testing.T) {
	m := wrapCRTestModel()
	const line = "Downloading 10%\rDownloading 100%\rdone\r\n"
	got := strings.Join(m.wrapLine(line), "\n")
	if strings.ContainsRune(got, '\r') {
		t.Fatalf("CR survived: %q", got)
	}
	if want := "Downloading 10%Downloading 100%done"; got != want {
		t.Fatalf("content changed: got %q, want %q", got, want)
	}
}

// End-to-end over the path that actually failed: a verbose execute_command
// result carrying CRLF and progress-bar CRs, sidebar visible. The rendered
// frame must be free of CRs (so nothing can repaint over the sidebar), and
// the sidebar border must still be the leftmost cell of its rows.
func TestVerboseToolResultCRKeepsSidebarIntact(t *testing.T) {
	m := newSidebarTestModel(100)
	m.SetSize(100, 24)
	m.sidebarVisible = true
	m.sidebarWidth = defaultSidebarWidth
	m.verbose = true

	// What execute_command returns for a CRLF/progress-emitting command.
	const result = "Downloading  10%\rDownloading 100%\rdone\r\nbuilding...\r\n"
	m.handleStreamToolResult("c1", "execute_command", result, true)

	frame := m.View().Content
	if strings.ContainsRune(frame, '\r') {
		t.Fatalf("tool result CR reached the frame (renderer would repaint from column 0):\n%q", frame)
	}
	// The transcript still shows the command output.
	if plain := ansi.Strip(frame); !strings.Contains(plain, "Downloading  10%") ||
		!strings.Contains(plain, "building...") {
		t.Fatalf("tool output lost from the transcript:\n%s", ansi.Strip(frame))
	}
}

// Same guard for the non-verbose path, where the mid-line CR comes from
// summarizeResult (TrimSpace only removes a TRAILING one, so a progress
// bar's mid-line CR used to survive and reach the frame).
func TestNonVerboseToolResultCRKeepsSidebarIntact(t *testing.T) {
	m := newSidebarTestModel(100)
	m.SetSize(100, 24)
	m.sidebarVisible = true
	m.sidebarWidth = defaultSidebarWidth
	m.verbose = false

	m.handleStreamToolResult("c1", "execute_command", "Downloading 10%\rDownloading 100%\rdone", true)

	frame := m.View().Content
	if strings.ContainsRune(frame, '\r') {
		t.Fatalf("summary CR reached the frame (renderer would repaint from column 0):\n%q", frame)
	}
}

// historyCRMessages builds a restored-session transcript whose tool result
// carries CRLF and progress-bar CRs, the shape a resumed session replays.
func historyCRMessages() []llm.Message {
	return []llm.Message{
		{Role: "user", Content: "run the build"},
		{Role: "assistant", ToolCalls: []llm.ToolCall{{
			ID:   "c1",
			Name: "execute_command",
			Args: map[string]any{"command": "go build ./..."},
		}}},
		{Role: "tool", ToolCallID: "c1", Content: "Downloading 10%\rDownloading 100%\rdone\r\nbuilding...\r\n"},
	}
}

// The history path re-renders a restored session through renderMessages,
// which never goes through the live streaming funnel. It must be guarded
// too, or a resumed session with CR-bearing tool output breaks again.
func TestHistoryRenderCRIsStripped(t *testing.T) {
	m := newSidebarTestModel(100)
	m.SetSize(100, 24)

	m.chatLines = renderMessages(historyCRMessages(), "/tmp", "model", "act", false)
	m.setViewportContent()

	frame := m.renderMainColumn()
	if strings.ContainsRune(frame, '\r') {
		t.Fatalf("history-rendered CR reached the frame:\n%q", frame)
	}
}

// wrapLine stays on the streaming hot path (buildFromPrefix runs it for the
// live line on every ~32 ms token batch), so the CR check must stay cheap for
// the common case: no CR means the line is returned as-is, with no copy.
func TestNormalizeCRFastPathDoesNotAllocate(t *testing.T) {
	line := strings.Repeat("ordinary assistant output line ", 4)
	if got := normalizeCR(line); got != line {
		t.Fatalf("no-CR line changed: %q", got)
	}
	// "Returned as-is, with no copy" is asserted by the string's data pointer,
	// NOT by testing.AllocsPerRun: AllocsPerRun reads the process-wide mallocs
	// counter, so anything else allocating in this test binary during the
	// measurement window makes a fast path that never allocated look guilty
	// (it read 299 allocs/run on a loaded machine, 0 on an idle one). Pointer
	// identity is exact and independent of load and of -race.
	if unsafe.StringData(normalizeCR(line)) != unsafe.StringData(line) {
		t.Fatal("normalizeCR copied a CR-free line; the fast path must return it as-is")
	}
}
