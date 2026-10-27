package tui

import (
	"strings"
	"testing"

	"gogen/internal/streambuf"
)

// wantThinkingLine computes the thinking line the OLD full-rebuild
// algorithm produced: renderStyledBlock over the whole buffer, then the
// wrap buildFromPrefix applies to the last chat line. It is the
// byte-identity reference for the incremental cache.
func wantThinkingLine(m *Model) string {
	displayBuf := strings.TrimRight(m.streamThinkingBuf.String(), "\n")
	styled := renderStyledBlock(ThinkingStyle, "<thinking>"+displayBuf)
	return strings.Join(m.wrapLine(styled), "\n")
}

// thinkingProbeText exercises every path the cache must match: word wrap,
// a hard-wrapped overlong token, a mid-block blank line, a trailing
// newline, and a final line without one.
const thinkingProbeText = "Let me think about this carefully.\n" +
	"First word wrap check: concurrencyconcurrencyconcurrency end.\n" +
	"Second line with a simple sentence.\n" +
	"\n" +
	"Third line.\n" +
	"Tail"

// streamThinkingChunked feeds text to handleStreamThinking in small
// chunks (sizes 1..3) and asserts after EVERY chunk that the rendered
// viewport content is byte-identical to the old full-rebuild reference.
func streamThinkingChunked(t *testing.T, m *Model, text string) {
	t.Helper()
	for offset, size := 0, 1; offset < len(text); size = size%3 + 1 {
		end := offset + size
		if end > len(text) {
			end = len(text)
		}
		m.handleStreamThinking(text[offset:end])
		offset = end
		if got, want := m.transcriptText(), wantThinkingLine(m); got != want {
			t.Fatalf("after %d bytes:\n got %q\nwant %q", offset, got, want)
		}
	}
}

// TestThinkingIncrementalMatchesFullRebuild pins the incremental cache to
// the old rebuild-from-buffer output, byte for byte, on a narrow viewport
// where word-wrap and hard-wrap both fire.
func TestThinkingIncrementalMatchesFullRebuild(t *testing.T) {
	m := newStreamTestModel()
	m.viewport.Width = 24 // force word-wrap

	streamThinkingChunked(t, m, thinkingProbeText)
	m.closeThinkingBlock()
	if m.streamThinkingCache != (streamThinkingCache{}) {
		t.Fatal("cache not cleared on close")
	}
}

// TestThinkingIncrementalTrailingNewline covers a buffer that ends in a
// newline mid-stream: the display must show no blank trailing line, and
// the next text token must start a NEW line (the trimmed newline stays
// pending in the buffer).
func TestThinkingIncrementalTrailingNewline(t *testing.T) {
	m := newStreamTestModel()
	m.viewport.Width = 24

	streamThinkingChunked(t, m, "alpha\nbeta\ngamma\n")
	m.handleStreamThinking("delta")
	if got, want := m.transcriptText(), wantThinkingLine(m); got != want {
		t.Fatalf("got %q\nwant %q", got, want)
	}
	if !strings.Contains(m.transcriptText(), "delta") {
		t.Fatalf("delta missing from display:\n%s", m.transcriptText())
	}
	m.closeThinkingBlock()
}

// TestThinkingIncrementalResizeRebuildsCache covers the width-change path:
// a resize mid-stream (setViewportContent) must rebuild the cache so the
// cached finalized lines re-wrap at the new width.
func TestThinkingIncrementalResizeRebuildsCache(t *testing.T) {
	m := newStreamTestModel()
	m.width = 100
	m.viewport.Width = 60

	streamThinkingChunked(t, m, thinkingProbeText)

	// Resize narrower mid-stream, then keep streaming.
	m.width = 80
	m.viewport.Width = 30
	m.setViewportContent()
	if got, want := m.transcriptText(), wantThinkingLine(m); got != want {
		t.Fatalf("after resize:\n got %q\nwant %q", got, want)
	}
	streamThinkingChunked(t, m, " more after resize\nfinal")
	m.closeThinkingBlock()
}

// TestThinkingIncrementalRoundBufferSeed covers the mid-turn join path:
// renderRoundBuffer seeds the buffer AND the cache from a snapshot; the
// subsequent live tokens must advance it to the same output a model that
// streamed everything live would show.
func TestThinkingIncrementalRoundBufferSeed(t *testing.T) {
	m := newStreamTestModel()
	m.width = 100 // let setViewportContent run its full rebuild
	m.viewport.Width = 24

	seed := "Seeded first line.\nSeeded second line with wrap."
	m.renderRoundBuffer(&streambuf.Snapshot{Thinking: seed})
	// renderRoundBuffer seeds the chat line with the (unwrapped) full
	// render, exactly as before the cache existed.
	if want := renderStyledBlock(ThinkingStyle, "<thinking>"+seed); m.chatLines[m.streamThinkingLine] != want {
		t.Fatalf("seeded line = %q, want %q", m.chatLines[m.streamThinkingLine], want)
	}
	m.setViewportContent()
	if got, want := m.transcriptText(), wantThinkingLine(m); got != want {
		t.Fatalf("after seed:\n got %q\nwant %q", got, want)
	}
	streamThinkingChunked(t, m, " continued\nand wrapped tightly here.")
	m.closeThinkingBlock()
}
