package tui

import (
	"strings"
	"testing"
	"unicode/utf8"

	"charm.land/bubbles/v2/key"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

func TestWrapLinePropagatesOpenStyles(t *testing.T) {
	const cyanBold = "\x1b[1;38;2;0;170;170m"
	const dim = "\x1b[38;2;136;136;136m"

	t.Run("thinking block — style open at split propagates", func(t *testing.T) {
		m := &Model{}
		m.viewport.Width = 42
		m.viewport.Style = ViewportStyle
		line := dim + "<thinking>" + strings.Repeat("reasoning text ", 8)
		parts := m.wrapLine(line)
		if len(parts) < 2 {
			t.Fatalf("expected wrapped parts, got %d", len(parts))
		}
		for i := 1; i < len(parts); i++ {
			if !strings.HasPrefix(parts[i], dim) {
				t.Fatalf("part %d missing dim prefix: %q", i, parts[i])
			}
			// Intermediate continuations are closed so padding cannot
			// inherit the style — accept either reset dialect. The LAST
			// part stays open when the source line does (same state as the
			// unwrapped input).
			if i < len(parts)-1 &&
				!strings.HasSuffix(parts[i], "\x1b[m") && !strings.HasSuffix(parts[i], "\x1b[0m") {
				t.Fatalf("part %d missing reset suffix: %q", i, parts[i])
			}
		}
	})

	t.Run("assistant label — closed style must NOT bleed onto continuations", func(t *testing.T) {
		// Regression for the lipgloss-v2 migration: v2 closes the label
		// with a bare "\x1b[m"; the old literal-reset heuristic treated the
		// style as open and painted every continuation line cyan/bold.
		m := &Model{}
		m.viewport.Width = 42
		m.viewport.Style = ViewportStyle
		line := cyanBold + "GoGen:\x1b[m " + strings.Repeat("plain reply text ", 10)
		parts := m.wrapLine(line)
		if len(parts) < 2 {
			t.Fatalf("expected wrapped parts, got %d", len(parts))
		}
		for i := 1; i < len(parts); i++ {
			if strings.Contains(parts[i], "38;2;0;170;170") {
				t.Fatalf("part %d leaked label style into body text: %q", i, parts[i])
			}
		}
	})

	t.Run("tool call — args style active at split propagates", func(t *testing.T) {
		m := &Model{}
		m.viewport.Width = 42
		m.viewport.Style = ViewportStyle
		line := "\x1b[38;2;204;170;0m  →\x1b[0m read_file " +
			dim + strings.Repeat("very long argument text ", 6) + "\x1b[0m"
		parts := m.wrapLine(line)
		if len(parts) < 3 {
			t.Fatalf("expected 3+ parts, got %d", len(parts))
		}
		for i := 2; i < len(parts); i++ {
			if !strings.HasPrefix(parts[i], dim) {
				t.Fatalf("part %d lost dim args styling: %q", i, parts[i])
			}
		}
	})

	t.Run("styled segment starts partway through a later line and itself spans 3+ wraps", func(t *testing.T) {
		// Regression test: a plain prefix + name is followed by a long
		// dimmed args value that only becomes active partway through the
		// second wrapped line, and itself needs several more wrapped
		// lines. Every wrapped line from the point the style first turns
		// up onward must carry it — previously the propagation logic only
		// looked at parts[0], so once the style appeared later than that,
		// none of the following continuation lines received it and the
		// styling silently vanished as soon as earlier lines scrolled out
		// of view.
		m := &Model{}
		m.viewport.Width = 42 // wrapWidth = 42 - 2 padding = 40
		m.viewport.Style = ViewportStyle

		dimSGR := "\x1b[38;2;136;136;136m"
		prefix := "\x1b[38;2;204;170;0m  →\x1b[0m read_file "
		args := dimSGR + strings.Repeat("very long argument text ", 6) + "\x1b[0m"
		line := prefix + args

		parts := m.wrapLine(line)
		if len(parts) < 4 {
			t.Fatalf("expected line to wrap into at least 4 parts, got %d: %#v", len(parts), parts)
		}

		seenStyle := false
		for i, p := range parts {
			if strings.Contains(p, dimSGR) {
				seenStyle = true
			}
			if seenStyle && p != "" && !strings.HasPrefix(p, "\x1b[") {
				t.Fatalf("part %d lost styling after the dim style should have carried forward: %q", i, p)
			}
		}
		if !seenStyle {
			t.Fatal("test setup issue: dim style never appeared in any wrapped part")
		}
	})
}

func TestWrapLineFitsWidth(t *testing.T) {
	m := &Model{}
	m.viewport.Width = 42 // wrapWidth = 42 - 2 padding = 40
	m.viewport.Style = ViewportStyle

	line := "See https://example.com/very/long/path/that/exceeds/forty/columns/easily for details"
	parts := m.wrapLine(line)
	w := m.wrapWidth()
	for i, p := range parts {
		plain := stripANSI(p)
		if got := len([]rune(plain)); got > w {
			t.Fatalf("part %d rune width %d > wrapWidth %d: %q", i, got, w, plain)
		}
	}
	if len(parts) < 3 {
		t.Fatalf("expected hard-wrap to split overlong URL, got %d parts: %#v", len(parts), parts)
	}
}

// TestCopySelectionBindingMatchesCtrlShiftC is the bubbletea-v2 migration
// regression test. On v1, ctrl+shift+c was byte-identical to ctrl+c in
// legacy terminal encoding (0x03), so pressing it quit the program instead
// of copying. v2 negotiates key disambiguation, delivering a distinct
// KeyPressMsg that the CopySelection binding must match — while plain
// ctrl+c keeps its cancel/quit role.
func TestCopySelectionBindingMatchesCtrlShiftC(t *testing.T) {
	tests := []struct {
		name string
		msg  tea.KeyPressMsg
		want bool
	}{
		{
			name: "ctrl+shift+c matches copy binding",
			msg:  tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl | tea.ModShift},
			want: true,
		},
		{
			name: "plain ctrl+c does not match copy binding",
			msg:  tea.KeyPressMsg{Code: 'c', Mod: tea.ModCtrl},
			want: false,
		},
		{
			name: "unmodified c does not match copy binding",
			msg:  tea.KeyPressMsg{Code: 'c'},
			want: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := key.Matches(tt.msg, DefaultKeyMap.CopySelection); got != tt.want {
				t.Fatalf("key.Matches(%q) = %v, want %v", tt.msg.String(), got, tt.want)
			}
		})
	}
}

func TestSliceByCellsUTF8(t *testing.T) {
	// "café" — é is multi-byte; cells 0..4 cover all 4 runes
	got := sliceByCells("café", 0, 4)
	if got != "café" {
		t.Fatalf("sliceByCells full = %q, want café", got)
	}
	got = sliceByCells("café", 3, 4)
	if got != "é" {
		t.Fatalf("sliceByCells last = %q, want é", got)
	}
	got = sliceByCells("café", 0, 3)
	if got != "caf" {
		t.Fatalf("sliceByCells prefix = %q, want caf", got)
	}
}

func TestGetSelectedTextUTF8(t *testing.T) {
	m := &Model{}
	seedViewportRows(m, "café au lait")
	m.selection = &SelectionState{
		Active: true,
		StartX: 0,
		StartY: 0,
		EndX:   4, // cells covering "café"
		EndY:   0,
	}
	got := m.getSelectedText()
	if got != "café" {
		t.Fatalf("getSelectedText = %q, want café", got)
	}
	if n := utf8.RuneCountInString(got); n != 4 {
		t.Fatalf("rune count = %d, want 4", n)
	}
}

func TestCopySelectionKeepsUntilCopy(t *testing.T) {
	m := &Model{}
	seedViewportRows(m, "hello world\nsecond line")
	m.selection = &SelectionState{
		Active:   true,
		Dragging: false,
		StartX:   0,
		StartY:   0,
		EndX:     5,
		EndY:     0,
	}
	if got := m.getSelectedText(); got != "hello" {
		t.Fatalf("getSelectedText = %q, want %q", got, "hello")
	}
	if !m.hasSelection() {
		t.Fatal("expected hasSelection")
	}
}

func TestMouseReleaseDoesNotClearSelection(t *testing.T) {
	m := &Model{}
	m.viewport.Height = 20
	m.viewport.Width = 40
	m.viewport.Style = ViewportStyle
	seedViewportRows(m, "hello world")
	m.selectionYOff = 0
	m.selection = &SelectionState{
		Active:   true,
		Dragging: true,
		StartX:   0,
		StartY:   0,
		EndX:     5,
		EndY:     0,
	}
	ev, ok := normalizeMouseEvent(tea.MouseReleaseMsg{X: 5, Y: 0, Button: tea.MouseLeft})
	if !ok {
		t.Fatal("expected release msg to normalize")
	}
	consumed := m.handleMouseSelection(ev)
	if !consumed {
		t.Fatal("expected release to be consumed")
	}
	if m.selection == nil || !m.selection.Active {
		t.Fatal("selection should remain after release")
	}
	if m.selection.Dragging {
		t.Fatal("dragging should be false after release")
	}
	if m.getSelectedText() != "hello" {
		t.Fatalf("selected text = %q", m.getSelectedText())
	}
}

func TestButtonNoneMotionDoesNotClearDrag(t *testing.T) {
	m := &Model{}
	m.viewport.Height = 20
	m.viewport.Width = 40
	m.viewport.Style = ViewportStyle
	seedViewportRows(m, "hello world")
	m.selectionYOff = 0
	m.selection = &SelectionState{
		Active:   true,
		Dragging: true,
		StartX:   0,
		StartY:   0,
		EndX:     5,
		EndY:     0,
	}
	// Some terminals emit button-less motion while the button is down / on release.
	ev, ok := normalizeMouseEvent(tea.MouseMotionMsg{X: 6, Y: 0, Button: tea.MouseNone})
	if !ok {
		t.Fatal("expected motion msg to normalize")
	}
	consumed := m.handleMouseSelection(ev)
	if !consumed {
		t.Fatal("expected motion to be consumed during drag")
	}
	if m.selection == nil || !m.selection.Active || !m.selection.Dragging {
		t.Fatal("ButtonNone motion must not clear an in-progress selection")
	}
	if m.getSelectedText() != "hello" {
		t.Fatalf("selected text = %q, want hello", m.getSelectedText())
	}
}

func TestMousePressRestartsSelection(t *testing.T) {
	m := &Model{}
	m.viewport.Height = 20
	m.viewport.Width = 40
	m.viewport.Style = ViewportStyle
	seedViewportRows(m, "hello world")
	m.selection = &SelectionState{
		Active:   true,
		Dragging: false,
		StartX:   0,
		StartY:   0,
		EndX:     5,
		EndY:     0,
	}
	// Left pad is 1 from ViewportStyle — click column 1+3 = content x 3
	leftPad := m.viewport.Style.GetPaddingLeft()
	ev, ok := normalizeMouseEvent(tea.MouseClickMsg{X: leftPad + 3, Y: 0, Button: tea.MouseLeft})
	if !ok {
		t.Fatal("expected click msg to normalize")
	}
	consumed := m.handleMouseSelection(ev)
	if !consumed {
		t.Fatal("expected press to be consumed")
	}
	if m.selection == nil || !m.selection.Dragging {
		t.Fatal("expected a new drag selection")
	}
	if m.selection.StartX != 3 || m.selection.StartY != 0 {
		t.Fatalf("start = (%d,%d), want (3,0)", m.selection.StartX, m.selection.StartY)
	}
}

// hasSelection reports whether there is a non-empty finalized or in-progress
// selection.
//
// Lives in this _test.go file: production code checks selection state via
// getSelectedText/clearSelection directly; only tests want the boolean
// summary.
func (m *Model) hasSelection() bool {
	return m.selection != nil && m.selection.Active && m.getSelectedText() != ""
}

// Regression: pointer events with a modal up acted underneath the opaque
// overlay — a press started a text selection in the covered transcript,
// wheel events scrolled it invisibly, and a border press started a panel
// drag. Only releases reach the selection handler now, so an in-flight
// drag finalizes and scrolling is not left locked after the modal closes.
func TestModalBlocksPointerUnderOverlay(t *testing.T) {
	m := newSidebarTestModel(100)
	m.sidebarVisible = false
	m.SetSize(100, 10)
	m.chatLines = []string{
		"You: filler line " + strings.Repeat("x", 40),
		"GoGen: reply " + strings.Repeat("y", 40),
		"You: more content " + strings.Repeat("z", 40),
		"GoGen: tail " + strings.Repeat("w", 40),
	}
	m.setViewportContent()
	m.viewport.GotoBottom()
	m.viewport.YOffset = 0
	m.modal = ModalHelp

	t.Run("press does not start a selection", func(t *testing.T) {
		m.handleMouseMsg(tea.MouseClickMsg{X: 10, Y: 3, Button: tea.MouseLeft})
		if m.selection != nil {
			t.Fatalf("press with a modal open started a selection under the overlay")
		}
	})

	t.Run("wheel does not scroll the covered chat", func(t *testing.T) {
		before := m.viewport.YOffset
		m.handleMouseMsg(tea.MouseWheelMsg{X: 10, Y: 3, Button: tea.MouseWheelDown})
		if m.viewport.YOffset != before {
			t.Fatalf("wheel scrolled the chat under the modal: %d -> %d", before, m.viewport.YOffset)
		}
	})

	t.Run("release finalizes an in-flight drag", func(t *testing.T) {
		m.modal = ModalNone
		m.handleMouseMsg(tea.MouseClickMsg{X: 10, Y: 1, Button: tea.MouseLeft})
		if m.selection == nil || !m.selection.Dragging {
			t.Fatalf("drag did not start")
		}
		m.modal = ModalHelp
		m.handleMouseMsg(tea.MouseReleaseMsg{X: 20, Y: 2, Button: tea.MouseLeft})
		// Either finalized in place or cleared (empty selection) — either
		// way Dragging must be off, so scrolling is unlocked afterwards.
		if m.selection != nil && m.selection.Dragging {
			t.Fatalf("release during a modal must finalize the drag (scroll stays unlocked)")
		}
		m.clearSelection()
	})

	t.Run("border press does not start a panel drag", func(t *testing.T) {
		m.modal = ModalNone
		m.sidebarVisible = true
		m.sidebarWidth = defaultSidebarWidth
		m.SetSize(100, 10)
		m.modal = ModalHelp
		m.handleMouseMsg(tea.MouseClickMsg{X: m.sidebarWidth - 1, Y: 3, Button: tea.MouseLeft})
		if m.sidebarDragging {
			t.Fatalf("border press with a modal open started a panel drag")
		}
	})
}

// Regression: the selection render cut [0, contentWidth) while the normal
// render cut [xOffset, xOffset+contentWidth) — with the viewport scrolled
// horizontally, the transcript visibly shifted the moment a drag started
// (and mouseToContent mapped columns without the offset).
func TestSelectionRenderMatchesHorizontalScroll(t *testing.T) {
	m := newSidebarTestModel(60)
	m.sidebarVisible = false
	m.SetSize(60, 10)
	long := "You: " + strings.Repeat("ab", 50) // 105 cells > the 56-cell content width
	m.chatLines = []string{long}
	m.setViewportContent()

	m.viewport.xOffset = 8
	m.viewport.longestLineWidth = ansi.StringWidth(stripANSI(long))

	normal := stripANSI(m.viewport.View())
	m.selection = &SelectionState{Active: true, StartX: 0, StartY: 0, EndX: 3, EndY: 0}
	withSel := stripANSI(m.renderViewportWithSelection())

	if normal != withSel {
		t.Fatalf("selection render shows a different window than the normal render:\n normal %q\n select %q",
			normal, withSel)
	}

	t.Run("mouse maps through the scroll offset", func(t *testing.T) {
		contentX, _ := m.mouseToContent(1, 1)
		if contentX != 8 {
			t.Fatalf("contentX = %d, want 8 (the xOffset)", contentX)
		}
	})
}

// seedViewportRows installs the given transcript text as the viewport's wrapped
// rows — the single rendered copy selection reads (the old wrappedContent/
// wrappedLines caches are gone).
func seedViewportRows(m *Model, text string) {
	rows := strings.Split(text, "\n")
	width := 0
	for _, r := range rows {
		if w := cellWidth(r); w > width {
			width = w
		}
	}
	m.viewport.SetContentLines(rows, width)
}

// TestSelectionRendersFromViewportRows pins the M1 rewrite: selection reads the
// viewport's wrapped rows (the only rendered copy) and derives plain text per
// visible row, instead of keeping a joined string plus its stripped and styled
// splits of the whole transcript.
func TestSelectionRendersFromViewportRows(t *testing.T) {
	m := dragModel(t)
	m.sidebarVisible = false
	m.viewport = NewViewport(m.mainWidth(), 10)
	m.viewport.Style = ViewportStyle
	// A styled row: highlighting must map plain cells back onto the styled
	// bytes, exactly as before the rewrite.
	m.chatLines = []string{AssistantStyle.Render("hello styled world"), "second line"}
	m.setViewportContent()

	plain := m.plainLine(0)
	if plain != "hello styled world" {
		t.Fatalf("plainLine(0) = %q, want the ANSI-stripped row", plain)
	}
	if got := m.plainLineCount(); got != len(m.viewport.Lines()) {
		t.Fatalf("plainLineCount = %d, want %d", got, len(m.viewport.Lines()))
	}

	m.selection = &SelectionState{Active: true, StartX: 0, StartY: 0, EndX: 5, EndY: 0}
	if got := m.getSelectedText(); got != "hello" {
		t.Fatalf("getSelectedText = %q, want %q", got, "hello")
	}
	rendered := m.renderViewportWithSelection()
	if !strings.Contains(rendered, "\x1b[7m") {
		t.Fatal("selection highlight missing from the rendered viewport")
	}
	if !strings.Contains(stripANSI(rendered), "hello styled world") {
		t.Fatalf("rendered viewport lost the row text: %q", stripANSI(rendered))
	}
}

// TestGetSelectedTextMultiRow uses viewport rows spanning a wrap boundary.
func TestGetSelectedTextMultiRow(t *testing.T) {
	m := &Model{}
	seedViewportRows(m, "first row\nsecond row\nthird")
	m.selection = &SelectionState{Active: true, StartX: 6, StartY: 0, EndX: 6, EndY: 1}
	if got, want := m.getSelectedText(), "row\nsecond"; got != want {
		t.Fatalf("getSelectedText = %q, want %q", got, want)
	}
}

// BenchmarkRenderViewportWithSelection covers the per-frame selection render on
// a long transcript. Before M1 this stripped and split the WHOLE transcript on
// every frame (two full copies); now it reads the viewport rows and strips only
// the visible selected rows.
func BenchmarkRenderViewportWithSelection(b *testing.B) {
	m := &Model{}
	m.sidebarVisible = false
	m.viewport = NewViewport(m.mainWidth(), 40)
	m.viewport.Style = ViewportStyle
	rows := make([]string, 2000)
	for i := range rows {
		rows[i] = AssistantStyle.Render("row " + strings.Repeat("x", 40))
	}
	m.viewport.SetContentLines(rows, 60)
	m.selection = &SelectionState{Active: true, StartX: 0, StartY: 100, EndX: 10, EndY: 130}
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if out := m.renderViewportWithSelection(); out == "" {
			b.Fatal("empty render")
		}
	}
}

// BenchmarkRenderViewportPlain is the unselected counterpart: the per-frame
// cost of Viewport.View on the same long transcript. The selection path pads
// its rows with fitRows too, so its cost should track this one rather than a
// whole lipgloss Render pass.
func BenchmarkRenderViewportPlain(b *testing.B) {
	m := &Model{}
	m.sidebarVisible = false
	m.viewport = NewViewport(m.mainWidth(), 40)
	m.viewport.Style = ViewportStyle
	rows := make([]string, 2000)
	for i := range rows {
		rows[i] = AssistantStyle.Render("row " + strings.Repeat("x", 40))
	}
	m.viewport.SetContentLines(rows, 60)
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if out := m.viewport.View(); out == "" {
			b.Fatal("empty render")
		}
	}
}

// TestSelectionSurvivesStreaming pins the B25 fix: appending rows (a token
// batch) must not wipe the user's selection. It used to be cleared on every
// batch, so text could not be selected while a turn streamed.
func TestSelectionSurvivesStreaming(t *testing.T) {
	m := newQueueTestModel()
	m.chatLines = []string{"first row"}
	m.SetSize(80, 40)
	m.streamAssistantLine = -1 // the real constructor's initial value
	m.selection = &SelectionState{Active: true, StartX: 0, StartY: 0, EndX: 5, EndY: 0}

	// A streaming append (appendChatLine) goes through buildFromPrefix.
	m.appendChatLine("second row")
	if m.selection == nil {
		t.Fatal("selection cleared by a streaming append")
	}
	// And the streamed-line path (publishStreamWrap) must not clear it either.
	m.handleStreamToken("streaming token")
	if m.selection == nil {
		t.Fatal("selection cleared by a token batch")
	}
	if got := m.getSelectedText(); got != "first" {
		t.Fatalf("selected text = %q, want the original selection", got)
	}

	// A full rebuild still clears: the rows are replaced, so row coordinates no
	// longer address the same text.
	m.setViewportContent()
	if m.selection != nil {
		t.Fatal("selection survived a full transcript rebuild")
	}
}

// TestZeroWidthRunesInSelection pins the cell math for combining marks and
// variation selectors: they occupy no cell of their own, so a selection must not
// shift by one per combining mark (and must still carry the mark when its base
// character is selected).
func TestZeroWidthRunesInSelection(t *testing.T) {
	const combining = "e\u0301" // e + COMBINING ACUTE

	m := &Model{}
	seedViewportRows(m, combining+"x")
	// Cell 0 is "é" (e + combining mark); cell 1 is "x".
	m.selection = &SelectionState{Active: true, StartX: 0, StartY: 0, EndX: 1, EndY: 0}
	if got, want := m.getSelectedText(), combining; got != want {
		t.Fatalf("selecting the composed cell = %q, want %q", got, want)
	}
	m.selection = &SelectionState{Active: true, StartX: 1, StartY: 0, EndX: 2, EndY: 0}
	if got, want := m.getSelectedText(), "x"; got != want {
		t.Fatalf("selecting the following cell = %q, want %q", got, want)
	}
	// The highlight's cell→rune mapping must cover the mark with its base.
	startRi, endRi := cellsToRuneRange(combining+"x", 0, 1)
	if startRi != 0 || endRi != 2 {
		t.Fatalf("cellsToRuneRange(0,1) = (%d,%d), want (0,2) to cover e+combining", startRi, endRi)
	}
}

// TestMapPlainToStyledSkipsOSC pins the OSC gap: a hyperlink/OSC sequence must be
// skipped like a CSI, or every mapping after it is off by the sequence length.
func TestMapPlainToStyledSkipsOSC(t *testing.T) {
	styled := "\x1b]8;;https://example.com\x07link\x1b[0m text"
	plain := stripANSI(styled)
	if plain != "link text" {
		t.Fatalf("plain = %q", plain)
	}
	// Plain index 5 ('t' of "text") maps to the byte offset of that 't'.
	got := mapPlainToStyled(5, styled)
	if got >= len(styled) || styled[got] != 't' {
		t.Fatalf("mapPlainToStyled(5) = %d (%q), want the 't' of \"text\"", got, styled[got:])
	}
}
