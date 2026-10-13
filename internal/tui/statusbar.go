package tui

import (
	"strings"

	"gogen/internal/contextmgr"
)

// The bar spans the MAIN COLUMN, not the terminal: with the sidebar
// visible the column is narrower, and an over-wide bar would grow the
// combined frame past the terminal width.
func (m *Model) renderStatusBar() string {
	w := m.mainWidth()
	if w <= 0 {
		return ""
	}
	if m.agent == nil {
		return StatusBarStyle.Width(w).Render("")
	}

	var leftParts []string

	// Transient status message (e.g. "Copied N chars") takes priority
	if m.statusMsg != "" {
		// Truncate to the bar's content area BEFORE centering: the styled
		// Width below wraps rather than clips, so an over-long message
		// (error text can be arbitrary — "✗ Compact failed: …", persist
		// warnings) would wrap to extra lines, grow the frame past the
		// terminal, and desync the inline renderer.
		avail := max(0, w-2) // -2 for the bar's horizontal padding
		msg := m.statusMsg
		if cellWidth(msg) > avail {
			msg = cutCells(msg, 0, avail)
		}
		content := StatusBarDimStyle.Render(msg)
		padLeft := max(0, (avail-cellWidth(msg))/2)
		result := strings.Repeat(" ", padLeft) + content
		return StatusBarStyle.Width(w).Render(result)
	}

	// Mode
	modeStr := m.agent.Mode.String()
	if modeStr == "plan" {
		leftParts = append(leftParts, StatusBarPlanStyle.Render("[plan]"))
	} else {
		leftParts = append(leftParts, StatusBarActStyle.Render("[act]"))
	}

	// Model
	if model := m.agent.CurrentModel(); model != "" {
		leftParts = append(leftParts, StatusBarDimStyle.Render(model))
	}

	// Thinking level (show only when non-off AND active for the current
	// model: an inactive stored value renders nothing, matching the web
	// toolbar's "no chip selected" state).
	if level := m.agent.ThinkingLevel; level != "" && level != "off" {
		if m.agent.IsThinkingLevelActive() {
			if short := level.ShortLabel(); short != "" {
				leftParts = append(leftParts, StatusBarDimStyle.Render("("+short+")"))
			}
		}
	}

	// Working directory / global indicator
	if m.agent.GlobalMode {
		leftParts = append(leftParts, StatusBarGlobalStyle.Render("🌐 global"))
	} else if wd := m.agent.WorkingDir; wd != "" {
		leftParts = append(leftParts, StatusBarDimStyle.Render(wd))
	}

	// Verbose indicator
	if m.verbose {
		leftParts = append(leftParts, StatusBarWarningStyle.Render("[verbose]"))
	}

	left := strings.Join(leftParts, " ")

	// Context line (right-aligned)
	right := ""
	if m.contextLine != "" {
		pct := 0
		if m.contextStats.Snapshot.Limit > 0 {
			pct = int(m.contextStats.Snapshot.Percent * 100)
		}
		style := StatusBarDimStyle
		if pct >= 90 {
			style = StatusBarDangerStyle
		} else if float64(pct) >= contextmgr.WarnThreshold*100 || m.contextStats.Snapshot.NearCompact {
			style = StatusBarWarningStyle
		}
		right = style.Render(m.contextLine)
	}

	// Layout: left and right with padding between. Prefer keeping the context
	// indicator visible — truncate the left side first when the bar is tight.
	availWidth := w - 2 // -2 for padding
	leftWidth := cellWidth(left)
	rightWidth := cellWidth(right)

	middleWidth := availWidth - leftWidth - rightWidth
	if middleWidth < 1 {
		keepRight := rightWidth
		if keepRight > availWidth-2 {
			keepRight = max(0, availWidth-2)
			right = cutCells(right, 0, keepRight)
			rightWidth = cellWidth(right)
		}
		maxLeft := max(0, availWidth-rightWidth-1)
		if leftWidth > maxLeft {
			left = cutCells(left, 0, maxLeft)
			leftWidth = cellWidth(left)
		}
		middleWidth = max(1, availWidth-leftWidth-rightWidth)
	}

	content := left + strings.Repeat(" ", middleWidth) + right

	return StatusBarStyle.Width(w).Render(content)
}
