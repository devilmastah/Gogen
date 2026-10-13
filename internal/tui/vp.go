package tui

import (
	"math"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

// Viewport is a custom scrollable viewport that tracks the longest line width
// incrementally to avoid O(N) scanning of the entire content on every update.
// It replaces the bubbles viewport for better rendering performance.
type Viewport struct {
	Width  int
	Height int

	// Whether to respond to mouse wheel events.
	MouseWheelEnabled bool

	// Number of lines scrolled per mouse wheel event (default 3).
	MouseWheelDelta int

	// YOffset is the vertical scroll position.
	YOffset int

	// Style applies a lipgloss style (borders, padding, margin) to the
	// viewport rendering.
	Style lipgloss.Style

	// Horizontal scroll step in columns (default 4).
	horizontalStep int

	lines            []string
	longestLineWidth int
	xOffset          int
}

// NewViewport returns a new Viewport with the given dimensions.
func NewViewport(width, height int) Viewport {
	return Viewport{
		Width:             width,
		Height:            height,
		MouseWheelEnabled: true,
		MouseWheelDelta:   1,
		horizontalStep:    4,
	}
}

// SetContent replaces the viewport content and computes the longest line width
// by scanning all lines.  Use SetContentMax when the max width is already
// known (e.g. during incremental streaming updates).
func (v *Viewport) SetContent(s string) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	v.lines = strings.Split(s, "\n")
	v.longestLineWidth = findLongestLineWidth(v.lines)

	if v.YOffset > len(v.lines)-1 {
		v.GotoBottom()
	}
}

// Lines returns the viewport's current line slice. The caller may reuse the
// backing array (e.g. splice in place) and must publish the result via
// SetContentLines before the viewport renders again.
func (v *Viewport) Lines() []string {
	return v.lines
}

// SetContentLines replaces the viewport content with pre-split lines and a
// pre-computed maximum line width, avoiding the O(N) scan and re-split of
// SetContent/SetContentMax. The caller is responsible for ensuring maxWidth
// is >= the actual longest line width. The slice is stored as-is; callers
// that reuse the backing array must not hold stale subslices across updates.
func (v *Viewport) SetContentLines(lines []string, maxWidth int) {
	v.lines = lines
	v.longestLineWidth = maxWidth

	if v.YOffset > len(v.lines)-1 {
		v.GotoBottom()
	}
}

// SetContentMax replaces the viewport content using a pre-computed maximum
// line width, avoiding an O(N) scan.  The caller is responsible for ensuring
// maxWidth is >= the actual longest line width.
func (v *Viewport) SetContentMax(s string, maxWidth int) {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	v.lines = strings.Split(s, "\n")
	v.longestLineWidth = maxWidth

	if v.YOffset > len(v.lines)-1 {
		v.GotoBottom()
	}
}

// TotalLines returns the number of lines stored in the viewport.
func (v Viewport) TotalLines() int {
	return len(v.lines)
}

// AtTop returns whether the viewport is scrolled to the top.
func (v Viewport) AtTop() bool {
	return v.YOffset <= 0
}

// AtBottom returns whether the viewport is scrolled to the bottom.
func (v Viewport) AtBottom() bool {
	return v.YOffset >= v.maxYOffset()
}

// ScrollPercent returns the amount scrolled as a float between 0 and 1.
func (v Viewport) ScrollPercent() float64 {
	if v.Height >= len(v.lines) {
		return 1.0
	}
	y := float64(v.YOffset)
	h := float64(v.Height)
	t := float64(len(v.lines))
	rv := y / (t - h)
	return math.Max(0.0, math.Min(1.0, rv))
}

// SetYOffset sets the vertical scroll offset, clamped to valid range.
func (v *Viewport) SetYOffset(n int) {
	v.YOffset = clamp(n, 0, v.maxYOffset())
}

// GotoTop scrolls to the top.
func (v *Viewport) GotoTop() {
	v.YOffset = 0
}

// GotoBottom scrolls to the bottom.
func (v *Viewport) GotoBottom() {
	v.YOffset = v.maxYOffset()
}

// LineUp scrolls up by n lines.
func (v *Viewport) LineUp(n int) {
	v.SetYOffset(v.YOffset - n)
}

// LineDown scrolls down by n lines.
func (v *Viewport) LineDown(n int) {
	v.SetYOffset(v.YOffset + n)
}

// PageUp scrolls up by one viewport height.
func (v *Viewport) PageUp() {
	v.LineUp(v.Height)
}

// PageDown scrolls down by one viewport height.
func (v *Viewport) PageDown() {
	v.LineDown(v.Height)
}

// HalfPageUp scrolls up by half the viewport height.
func (v *Viewport) HalfPageUp() {
	v.LineUp(v.Height / 2) //nolint:mnd
}

// HalfPageDown scrolls down by half the viewport height.
func (v *Viewport) HalfPageDown() {
	v.LineDown(v.Height / 2) //nolint:mnd
}

// maxYOffset returns the maximum valid Y offset.
func (v Viewport) maxYOffset() int {
	return max(0, len(v.lines)-v.Height+v.Style.GetVerticalFrameSize())
}

// visibleLines returns the lines that should currently be visible.
func (v Viewport) visibleLines() []string {
	h := v.Height - v.Style.GetVerticalFrameSize()
	w := v.Width - v.Style.GetHorizontalFrameSize()

	if len(v.lines) == 0 {
		return nil
	}

	top := max(0, v.YOffset)
	bottom := clamp(v.YOffset+h, top, len(v.lines))
	lines := v.lines[top:bottom]

	// If no horizontal scroll is needed, return as-is.
	if (v.xOffset == 0 && v.longestLineWidth <= w) || w == 0 {
		return lines
	}

	cutLines := make([]string, len(lines))
	for i := range lines {
		cutLines[i] = cutCells(lines[i], v.xOffset, v.xOffset+w)
	}
	return cutLines
}

// View renders the viewport into a string suitable for display.
func (v Viewport) View() string {
	w, h := v.Width, v.Height
	if sw := v.Style.GetWidth(); sw != 0 {
		w = min(w, sw)
	}
	if sh := v.Style.GetHeight(); sh != 0 {
		h = min(h, sh)
	}
	contentWidth := w - v.Style.GetHorizontalFrameSize()
	contentHeight := h - v.Style.GetVerticalFrameSize()
	contents := lipgloss.NewStyle().
		Width(contentWidth).
		Height(contentHeight).
		MaxHeight(contentHeight).
		MaxWidth(contentWidth).
		Render(strings.Join(v.visibleLines(), "\n"))
	return v.Style.
		UnsetWidth().UnsetHeight().
		Render(contents)
}

// Update handles tea.Msg for mouse wheel scrolling.
func (v Viewport) Update(msg tea.Msg) (Viewport, tea.Cmd) {
	if !v.MouseWheelEnabled {
		return v, nil
	}

	// v2: wheel events arrive as a dedicated MouseWheelMsg; the direction
	// is carried in Button.
	mouseMsg, ok := msg.(tea.MouseWheelMsg)
	if !ok {
		return v, nil
	}

	switch {
	case mouseMsg.Button == tea.MouseWheelUp:
		v.LineUp(v.MouseWheelDelta)
	case mouseMsg.Button == tea.MouseWheelDown:
		v.LineDown(v.MouseWheelDelta)
	case mouseMsg.Button == tea.MouseWheelLeft:
		v.scrollLeft(v.horizontalStep)
	case mouseMsg.Button == tea.MouseWheelRight:
		v.scrollRight(v.horizontalStep)
	}

	return v, nil
}

func (v *Viewport) scrollLeft(n int) {
	v.xOffset = clamp(v.xOffset-n, 0, v.longestLineWidth-v.Width)
}

func (v *Viewport) scrollRight(n int) {
	v.xOffset = clamp(v.xOffset+n, 0, v.longestLineWidth-v.Width)
}

// findLongestLineWidth scans lines and returns the maximum visual width.
func findLongestLineWidth(lines []string) int {
	w := 0
	for _, l := range lines {
		if ww := cellWidth(l); ww > w {
			w = ww
		}
	}
	return w
}

// clamp returns v clamped to [low, high].
func clamp(v, low, high int) int {
	if high < low {
		low, high = high, low
	}
	return min(high, max(low, v))
}
