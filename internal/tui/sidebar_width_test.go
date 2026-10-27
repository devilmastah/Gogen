package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"gogen/internal/agent"
)

// TestSidebarRowsMeasureTheCutCells is the B25d regression: each panel row is
// cut with ansi.Cut but was padded by the UNCUT width, so a label whose wide
// rune (CJK, emoji) straddled the boundary produced a row one cell short and
// the right border stepped left on exactly those lines.
//
// The invariant under test is the panel's column budget: every rendered line,
// borders included, is exactly sidebarWidth cells wide.
func TestSidebarRowsMeasureTheCutCells(t *testing.T) {
	// The wide-rune path that actually reaches the padding helper: the panel's
	// working-directory row (the label row has its own correct budget maths in
	// renderSidebarTitle).
	label := "日本語のラベルがとても長い場合" // 15 double-width runes
	for w := 24; w <= 34; w++ {
		t.Run(fmt.Sprintf("width=%d", w), func(t *testing.T) {
			m := newSidebarFullModel(t)
			m.sidebarMainLines = 30
			m.sidebarWidth = w
			m.agent.WorkingDir = "/home/日本/プロジェクト/とても長いディレクトリ"
			m.savedCache = []agent.SessionInfo{{
				ID:        "wide",
				Label:     label,
				UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
			}}
			lines := strings.Split(m.renderSidebar(30), "\n")
			if len(lines) < 3 {
				t.Fatalf("panel rendered %d lines: %#v", len(lines), lines)
			}
			for i, line := range lines {
				if got := cellWidth(line); got != w {
					t.Fatalf("line %d is %d cells, want %d: %q", i, got, w, stripANSI(line))
				}
			}
		})
	}
}
