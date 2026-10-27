package tui

import (
	"fmt"

	"charm.land/bubbles/v2/spinner"
	tea "charm.land/bubbletea/v2"
	"github.com/charmbracelet/x/ansi"
)

// progressPhase controls the input-area wait indicator.
// Spinner animates only for idle waits; token/tool-arg streaming is the progress.
type progressPhase int

const (
	progressHidden     progressPhase = iota // not in a turn
	progressThinking                        // waiting on the model
	progressActive                          // tokens / tool args flowing in chat
	progressTool                            // a tool is executing
	progressCompacting                      // mid-turn compaction: silent summarization call
)

// Pre-rendered static progress lines so DimStyle.Render is not called every
// frame for content that never changes between renders.
var (
	progressStreamingLine = DimStyle.Render("  streaming…")
)

func newProgressSpinner() spinner.Model {
	return spinner.New(
		spinner.WithSpinner(spinner.MiniDot),
		spinner.WithStyle(DimStyle),
	)
}

func (m *Model) progressAnimating() bool {
	return m.streaming && (m.progressPhase == progressThinking || m.progressPhase == progressTool || m.progressPhase == progressCompacting)
}

// focusedSession returns the focused live session, or nil when the Model
// has no registry (unit-test constructions). Progress mirroring is a
// no-op in that case.
func (m *Model) focusedSession() *liveSession {
	if m.lives == nil {
		return nil
	}
	return m.lives.Active()
}

// setProgress updates the wait indicator. Returns a spinner tick when animation
// should (re)start after being stopped.
//
// The phase/label are mirrored onto the focused live session so the state
// survives a focus switch: switchToLive restores the target session's
// recorded phase instead of hardcoding "thinking".
func (m *Model) setProgress(phase progressPhase, label string) tea.Cmd {
	wasAnimating := m.progressAnimating()
	m.progressPhase = phase
	m.progressLabel = label
	// Any normal progress update supersedes a retry label: the retry either
	// delivered output (tokens/stats arrive) or the round moved on, so the
	// stall signal may relabel again. handleStreamRetryMsg re-sets the flag
	// right after its own setProgress call.
	m.progressRetry = false
	if s := m.focusedSession(); s != nil {
		s.progressPhase = phase
		s.progressLabel = label
		s.progressRetry = false
	}
	if m.progressAnimating() && !wasAnimating {
		return m.spinner.Tick
	}
	return nil
}

// setActiveTool names the tool being prepared/executed for the progress
// indicator; mirrored onto the focused session like setProgress.
func (m *Model) setActiveTool(name string) {
	m.activeToolName = name
	if s := m.focusedSession(); s != nil {
		s.activeTool = name
	}
}

func (m *Model) clearProgress() {
	m.progressPhase = progressHidden
	m.progressLabel = ""
	m.progressRetry = false
	// No tool is being prepared/executed any more (turn end, cancel, error).
	m.activeToolName = ""
	m.streamSpeedLine = ""
	if s := m.focusedSession(); s != nil {
		s.resetProgress()
	}
}

// fitStripRow cuts a busy-strip row to the main column's width. The strip
// renders as ONE row inside the input band (renderMainColumn), but its
// content is unbounded by construction: tool names ("running
// mcp__server__very_long_tool…"), stream-retry reasons, the token rate,
// and the queued-messages suffix all concatenate into one line. An
// over-wide row makes JoinVertical pad every frame row to its width — the
// frame grows past the terminal, soft-wraps, and the composer and status
// bar end up below the bottom edge. Widths ≤ 0 (literal-built test models
// before SetSize) are left alone.
func (m *Model) fitStripRow(line string) string {
	w := m.mainWidth()
	if w <= 0 || cellWidth(line) <= w {
		return line
	}
	line = cutCells(line, 0, w)
	// Cut preserves escape sequences but drops the closer that sat past the
	// cut point — and SGR carries across newlines, so an open tail would
	// bleed the strip's style into the composer row below it.
	if openSGRAt(line, w) != "" {
		line += ansi.ResetStyle
	}
	return line
}

// renderProgressInput draws the wait indicator as the input band's ONE
// progress row. The composer renders below it (renderMainColumn): since
// steering, the textarea stays visible and editable while a turn runs —
// Enter queues, ctrl+c interrupts — and gives the strip one of its rows,
// so the band's total height (and the viewport's) never changes at turn
// boundaries (SetSize reserves the strip row while streaming/compacting).
func (m *Model) renderProgressInput() string {
	var line string
	switch m.progressPhase {
	case progressThinking:
		label := m.progressLabel
		if label == "" {
			label = "thinking"
		}
		// The spinner + label are the busy indicator while the model works
		// up to its first token — without them the strip renders an EMPTY
		// row (the rate below is empty until the round's first stats
		// message, so there would be nothing on screen at all).
		line = DimStyle.Render("  " + m.spinner.View() + " " + label)
		// Token rate from the shared SpeedMeter (thinking tokens count
		// toward it too); appended once the round's stats arrive, so the
		// indicator never shows a stale rate.
		if m.streamSpeedLine != "" {
			line += " " + m.streamSpeedLine
		}
	case progressTool:
		label := m.progressLabel
		if label == "" {
			label = "running tool"
		}
		line = DimStyle.Render("  " + m.spinner.View() + " " + label)
	case progressCompacting:
		// Mid-turn compaction (auto/forced, inside prepareMessages): the
		// summarization is a full non-streaming LLM call with no callbacks,
		// so the label must say what the silence is. No rate is shown — the
		// SpeedMeter is not fed by the compacting request.
		label := m.progressLabel
		if label == "" {
			label = "compacting history"
		}
		line = DimStyle.Render("  " + m.spinner.View() + " " + label)
	case progressActive:
		// Name the tool whose arguments are streaming in, so the long
		// pre-execution stretch of tools like patch_file is not opaque.
		if m.activeToolName != "" {
			line = DimStyle.Render("  preparing " + m.activeToolName + "…")
		} else {
			line = progressStreamingLine
		}
		// Token rate from the shared SpeedMeter (rendered once per stats
		// message on the Update thread, not per frame).
		if m.streamSpeedLine != "" {
			line += " " + m.streamSpeedLine
		}
	default:
		// Fallback for progressHidden (renderProgressInput is only called
		// when streaming is true, but handle defensively).
		line = ""
	}
	return m.fitStripRow(line + m.queuedSuffix())
}

// renderCompactingInput draws the /compact wait indicator row. The
// compaction runs off the Update thread; the spinner animates via
// handleSpinnerTick, which also ticks while compacting.
func (m *Model) renderCompactingInput() string {
	return m.fitStripRow(DimStyle.Render("  "+m.spinner.View()+" compacting history…") + m.queuedSuffix())
}

// queuedSuffix is the input band's queued-messages count ("· N queued") —
// the focused session's steering queue, mirrored like the progress fields
// (switchToLive restores it on focus). Zero items → empty suffix.
func (m *Model) queuedSuffix() string {
	s := m.focusedSession()
	if s == nil || s.queueLen() == 0 {
		return ""
	}
	return DimStyle.Render(fmt.Sprintf("  · %d queued", s.queueLen()))
}
