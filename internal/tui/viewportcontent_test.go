package tui

import (
	"slices"
	"strings"
	"testing"
)

// TestSetViewportContentSinglePassParity pins the invariants the single-pass
// full rebuild relies on: the viewport receives the per-line wrap flatten
// directly (no join→split round-trip), the incremental prefix is a slice of
// that flatten instead of a second wrap pass, prefixLines+lastWrapped
// reassemble exactly the viewport rows (the viewport is the ONLY rendered
// copy now), and the aliased prefixLines survive the streaming append funnel
// (appendChatLine writes the old last line's rows into the shared backing array
// before buildFromPrefix republishes).
func TestSetViewportContentSinglePassParity(t *testing.T) {
	m := dragModel(t)
	m.sidebarVisible = false
	m.viewport = NewViewport(m.mainWidth(), 20)
	m.viewport.Style = ViewportStyle

	long := strings.Repeat("word ", 30) // wraps into several parts at chat width
	m.chatLines = []string{
		UserStyle.Render(userLabel) + " first prompt",
		long,
		"middle line",
		UserStyle.Render(userLabel) + " second prompt",
		long + " tail",
	}
	m.setViewportContent()

	// Reference: deterministically re-wrap each chat line (what the removed
	// second loop used to recompute) and flatten.
	wrapAll := func(lines []string) []string {
		var parts []string
		for _, line := range lines {
			parts = append(parts, m.wrapLine(line)...)
		}
		return parts
	}

	if got, want := m.viewport.Lines(), wrapAll(m.chatLines); !slices.Equal(got, want) {
		t.Fatalf("viewport lines != per-line wrap flatten\ngot:  %q\nwant: %q", got, want)
	}

	wantPrefix := wrapAll(m.chatLines[:len(m.chatLines)-1])
	if !slices.Equal(m.prefixLines, wantPrefix) {
		t.Fatalf("prefixLines != chatLines[:len-1] wrap\n got: %q\nwant: %q", m.prefixLines, wantPrefix)
	}
	if got, want := strings.Join(append(slices.Clone(m.prefixLines), strings.Split(m.lastWrapped, "\n")...), "\n"), strings.Join(m.viewport.Lines(), "\n"); got != want {
		t.Fatalf("prefix+lastWrapped != viewport rows\ngot:  %q\nwant: %q", got, want)
	}

	// The aliased prefix must survive the append funnel.
	m.appendChatLine("appended " + long)
	if got, want := m.viewport.Lines(), wrapAll(m.chatLines); !slices.Equal(got, want) {
		t.Fatalf("viewport lines after append funnel != per-line wrap flatten\ngot:  %q\nwant: %q", got, want)
	}
	if got, want := m.viewport.Lines(), append(slices.Clone(m.prefixLines), m.wrapLine(m.chatLines[len(m.chatLines)-1])...); !slices.Equal(got, want) {
		t.Fatalf("viewport lines != prefixLines+last after append funnel\ngot:  %q\nwant: %q", got, want)
	}
}
