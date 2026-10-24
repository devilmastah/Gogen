package tui

import (
	"image/color"
	"os"

	"charm.land/lipgloss/v2"
)

var (
	noColor bool
)

func init() {
	noColor = os.Getenv("NO_COLOR") != ""
	if noColor {
		ansiHighlightOn = ""
		ansiDimOn = ""
		ansiPromptOn = ""
		ansiCyanOn = ""
		ansiGreenOn = ""
		ansiRedOn = ""
		ansiReset = ""
	} else {
		ansiHighlightOn = "\x1b[1m\x1b[38;2;0;170;170m"
		ansiDimOn = "\x1b[38;2;136;136;136m"
		ansiPromptOn = "\x1b[38;2;204;170;0m"
		ansiCyanOn = "\x1b[38;2;0;170;170m"
		ansiGreenOn = "\x1b[38;2;0;170;0m"
		ansiRedOn = "\x1b[38;2;200;60;60m"
		// Only reset foreground + bold, never background.
		// This keeps the overlay's #1a1a1a background alive past
		// the end of styled text, preventing black gaps and bleed.
		ansiReset = "\x1b[39m\x1b[22m"
	}
}

// Raw ANSI prefixes for modal rendering.  We emit only the *start* of
// each sequence inside a border box; ansiReset (foreground+bold only,
// never background) follows the text.  This keeps the overlay's #1a1a1a
// background alive through the entire line.
var (
	ansiHighlightOn string // bold cyan
	ansiDimOn       string // gray
	ansiPromptOn    string // yellow
	ansiCyanOn      string // cyan (no bold)
	ansiGreenOn     string // green (sidebar "active" state dot)
	ansiRedOn       string // red (failed subagent child dot)
	ansiReset       string // \x1b[39m\x1b[22m — fg+bold only
)

func lipglossColor(hex string) color.Color {
	if noColor {
		return lipgloss.Color("")
	}
	return lipgloss.Color(hex)
}

// Shared lipgloss styles for the TUI.
var (
	// Chat viewport
	ViewportStyle = lipgloss.NewStyle().
			PaddingLeft(1).
			PaddingRight(1)

	// Message roles
	AssistantStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipglossColor("#00AAAA"))

	SystemStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#888888"))

	DimStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#888888"))

	UserStyle = lipgloss.NewStyle().
			Bold(true).
			Foreground(lipglossColor("#CCAA00"))

	// Thinking blocks
	ThinkingTagStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#888888"))

	ThinkingStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#888888"))

	// Tool calls
	ToolCallStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#CCAA00"))

	ToolCallArgsStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#888888"))

	// Tool results
	ToolResultOKStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#00AA00"))

	ToolResultFailStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#AA0000"))

	ToolResultBodyStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#888888"))

	ToolResultMarkStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#888888"))

	// Status bar
	StatusBarStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#FFFFFF")).
			Background(lipglossColor("#333333")).
			Padding(0, 1).
			Width(0) // filled by layout

	StatusBarDimStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#AAAAAA")).
				Background(lipglossColor("#333333"))

	StatusBarWarningStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#CCAA00")).
				Background(lipglossColor("#333333"))

	StatusBarDangerStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#AA0000")).
				Background(lipglossColor("#333333"))

	StatusBarPlanStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#CCAA00")).
				Background(lipglossColor("#333333"))

	StatusBarActStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#00AA00")).
				Background(lipglossColor("#333333"))

	StatusBarGlobalStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#00AAAA")).
				Background(lipglossColor("#333333"))

	// Modals
	ModalOverlayStyle = lipgloss.NewStyle().
				Background(lipglossColor("#000000")).
				Foreground(lipglossColor("#FFFFFF"))

	// Background fill for modal overlay (cached to avoid allocation per frame).
	ModalOverlayBackground = lipgloss.NewStyle().
				Background(lipglossColor("#1a1a1a"))

	// Error
	ErrorStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#AA0000"))

	// Input
	InputPromptStyle = lipgloss.NewStyle().
				Foreground(lipglossColor("#00AAAA"))

	// Divider
	DividerStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#555555"))

	// Diff rendering
	DiffAddStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#00AA00"))

	DiffDelStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#AA0000"))

	DiffHunkStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#00AAAA"))

	DiffMetaStyle = lipgloss.NewStyle().
			Foreground(lipglossColor("#AAAA00")).
			Bold(true)
)
