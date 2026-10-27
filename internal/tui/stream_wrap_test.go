package tui

import (
	"fmt"
	"strings"
	"testing"
)

// TestStreamWrapMatchesFullRewrap pins the incremental assistant wrap against
// the authoritative path: the viewport produced while streaming (per-line cache)
// must be byte-identical to a full re-wrap of the same raw line. That equality
// is what makes the cache an optimization rather than a behaviour change — the
// corpus below covers what the wrap pipeline actually transforms: wrapping,
// blank lines, a tab (expanded before wrapping), a bare CR (dropped) and a
// token that has to be hard-wrapped.
func TestStreamWrapMatchesFullRewrap(t *testing.T) {
	m := newQueueTestModel()
	m.chatLines = nil
	m.SetSize(80, 40)

	chunks := []string{
		"The quick brown ",
		"fox jumps over the lazy dog\n",
		"second line, long enough to wrap at least once in this viewport width\n",
		"\n",
		"tab\there car\rriage return and ",
		"aVeryLongTokenWithoutAnyBreakPointsThatMustBeHardWrappedSomewhere ",
		"tail",
	}
	for _, c := range chunks {
		m.handleStreamToken(c)
	}
	streamed := strings.Join(m.viewport.Lines(), "\n")

	// Rebuild from the raw chat line through the full path.
	m.setViewportContent()
	full := strings.Join(m.viewport.Lines(), "\n")

	if streamed != full {
		t.Fatalf("incremental wrap diverged from the full re-wrap\n--- streamed ---\n%s\n--- full ---\n%s", streamed, full)
	}
	// Sanity: the reply really did wrap and break (otherwise the comparison is
	// vacuous).
	if n := len(m.viewport.Lines()); n < 4 {
		t.Fatalf("expected a multi-row reply, got %d rows", n)
	}
}

// TestStreamWrapSeedsAfterInvalidation: a write that does not go through the
// streaming path (the round-end trim, a transcript rebuild at a new width) must
// leave the next batch correct, so the cache re-seeds from raw text.
func TestStreamWrapSeedsAfterInvalidation(t *testing.T) {
	m := newQueueTestModel()
	m.chatLines = nil
	m.SetSize(80, 40)
	m.handleStreamToken("first line\nsecond ")
	// A width change invalidates the cached wrap (the seams move).
	m.SetSize(50, 40)
	m.handleStreamToken("line continues")
	streamed := strings.Join(m.viewport.Lines(), "\n")
	m.setViewportContent()
	if full := strings.Join(m.viewport.Lines(), "\n"); streamed != full {
		t.Fatalf("wrap after invalidation diverged\n--- streamed ---\n%s\n--- full ---\n%s", streamed, full)
	}
}

// BenchmarkStreamReply measures the streaming path end to end: a reply of
// `lines` wrapped lines fed in fixed 32-byte batches. The cost is linear in the
// reply because only the in-progress line is re-wrapped per batch (the old
// whole-line re-wrap made it quadratic: 4x the reply cost ~16x the time).
func BenchmarkStreamReply(b *testing.B) {
	for _, lines := range []int{50, 200} {
		b.Run(fmt.Sprintf("lines=%d", lines), func(b *testing.B) {
			reply := strings.Repeat("the quick brown fox jumps over the lazy dog\n", lines)
			batch := 32
			b.ReportAllocs()
			b.SetBytes(int64(len(reply)))
			for i := 0; i < b.N; i++ {
				m := newQueueTestModel()
				m.chatLines = nil
				m.SetSize(120, 40)
				for off := 0; off < len(reply); off += batch {
					end := min(off+batch, len(reply))
					m.handleStreamToken(reply[off:end])
				}
			}
		})
	}
}

// BenchmarkStreamReplyFullRewrap is the baseline the incremental cache
// replaces: the same batches, each appended through the generic write path,
// which re-wraps the whole accumulated reply (the pre-cache behaviour). The
// ratio between the two benchmarks is the cost the cache removes.
func BenchmarkStreamReplyFullRewrap(b *testing.B) {
	for _, lines := range []int{50, 200} {
		b.Run(fmt.Sprintf("lines=%d", lines), func(b *testing.B) {
			reply := strings.Repeat("the quick brown fox jumps over the lazy dog\n", lines)
			batch := 32
			b.ReportAllocs()
			b.SetBytes(int64(len(reply)))
			for i := 0; i < b.N; i++ {
				m := newQueueTestModel()
				m.chatLines = nil
				m.SetSize(120, 40)
				m.appendChatLine("assistant ")
				line := len(m.chatLines) - 1
				m.streamAssistantLine = line
				for off := 0; off < len(reply); off += batch {
					end := min(off+batch, len(reply))
					m.commitChatLineWrite(line, true, reply[off:end])
				}
			}
		})
	}
}
