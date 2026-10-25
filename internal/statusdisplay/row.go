package statusdisplay

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gogen/internal/agent"
)

// Display limits from the ttgo/status firmware (TITLE_MAX / MSG_MAX in
// status_logic.h). The device truncates the *fields* to these, but the bar
// LABEL is `title + "  " + message` trimmed to roughly 29 glyphs at the
// default font (labelMaxChars), so the payload keeps both strings short by
// design: the title is the constant "Gogen" and the message is just the
// activity plus the elapsed time.
const (
	titleMax = 48
	msgMax   = 96
)

// statusRow is the POST /status body the ttgo/status firmware expects. A
// progress of -1 means "no bar, label only" (an unresolved context window),
// matching the firmware's clampProgress.
type statusRow struct {
	Source   string  `json:"source"`
	Title    string  `json:"title,omitempty"`
	Message  string  `json:"message,omitempty"`
	Progress float64 `json:"progress"`
	State    string  `json:"state"`
	TTL      int     `json:"ttl,omitempty"`
}

// buildRow renders one turn event as the display's JSON row. The row is a
// full snapshot: every update re-states the title, message, context bar and
// state, so a dropped update costs nothing (the next one is complete).
// elapsed is the turn's duration (0 outside a turn): while a turn runs the
// message carries it, so the liveness ticker's refreshes visibly advance
// even when no agent event has arrived for minutes.
func buildRow(ev agent.TurnEvent, c Config, elapsed time.Duration) ([]byte, error) {
	used, limit, resolved := contextStats(ev.Agent)
	row := statusRow{
		Source:   c.Source,
		Title:    asciiOnly(titleFor(ev, c.workDir)),
		State:    "running",
		TTL:      runningTTLSeconds,
		Progress: -1,
	}
	if resolved {
		row.Progress = clamp01(float64(used) / float64(limit))
	}
	ctx := contextSuffix(used, limit, resolved)
	switch ev.Phase {
	case agent.TurnPhaseEnded:
		row.TTL = endedTTLSeconds
		switch {
		case ev.Err != nil && !ev.Cancelled:
			// The reason matters more than the context: no ctx suffix here,
			// so the visible part of the line is the error.
			row.State = "error"
			row.Message = asciiOnly("error: " + oneLine(ev.Err.Error()))
		case ev.Cancelled:
			row.State = "idle"
			row.Message = asciiOnly(appendElapsed("cancelled", elapsed))
		default:
			row.State = "done"
			row.Message = asciiOnly(joinContext(appendElapsed("done", elapsed), ctx))
		}
	default:
		row.Message = asciiOnly(joinContext(appendElapsed(activityFor(ev), elapsed), ctx))
	}
	row.Title = truncate(row.Title, titleMax)
	row.Message = truncate(row.Message, msgMax)
	return json.Marshal(row)
}

// joinContext appends the compact context suffix ("368k/1M") to a message
// when the window is known. The suffix goes LAST: the display trims the line
// from the right, so the activity and elapsed time survive a narrow bar.
func joinContext(msg, ctx string) string {
	if ctx == "" {
		return msg
	}
	if msg == "" {
		return ctx
	}
	return msg + " " + ctx
}

// contextSuffix renders the context usage compactly ("368k/1M"), or "" when
// the window is unresolved. The two-line display gives the message line ~28
// glyphs at the default font, shared with the activity and elapsed time.
func contextSuffix(used, limit int, resolved bool) string {
	if !resolved || limit <= 0 {
		return ""
	}
	return formatTokens(used) + "/" + formatTokens(limit)
}

// formatTokens renders a token count compactly (84200 -> "84.2k", 1000000 ->
// "1M") for the context suffix.
func formatTokens(n int) string {
	switch {
	case n >= 1_000_000:
		return strconv.Itoa(n/1_000_000) + "M"
	case n >= 100_000:
		return strconv.Itoa(n/1000) + "k"
	case n >= 1000:
		return strconv.FormatFloat(float64(n)/1000, 'f', 1, 64) + "k"
	default:
		return strconv.Itoa(n)
	}
}

// formatElapsed renders a turn duration compactly ("42s", "1m42s", "1h02m").
// Sub-second durations return "" — the first row of a turn does not need a
// "0s" suffix.
func formatElapsed(d time.Duration) string {
	if d < time.Second {
		return ""
	}
	s := int(d.Seconds())
	if s < 60 {
		return strconv.Itoa(s) + "s"
	}
	m := s / 60
	s %= 60
	if m < 60 {
		return fmt.Sprintf("%dm%02ds", m, s)
	}
	h := m / 60
	m %= 60
	return fmt.Sprintf("%dh%02dm", h, m)
}

// contextStats samples the agent's current context usage. resolved is false
// when there is no usable limit — the row then omits the bar rather than
// drawing a percentage of a made-up window.
func contextStats(a *agent.Agent) (used, limit int, resolved bool) {
	if a == nil {
		return 0, 0, false
	}
	snap := a.ContextStats(context.Background()).Snapshot
	if snap.Limit <= 0 {
		return snap.Used, 0, false
	}
	return snap.Used, snap.Limit, true
}

// titleFor is the row title: "Gogen@<project>: <session label>" when both
// are known, degrading to "Gogen@<project>" / "Gogen: <label>" / "Gogen".
// The display draws it on the FIRST label line (two-line mode, title above /
// message below) and trims it to the bar width; ASCII only, since the
// firmware's fonts have no glyph above 0x7E. The project tag is what tells
// concurrent Gogen instances apart on the panel.
func titleFor(ev agent.TurnEvent, workDir string) string {
	base := "Gogen"
	if tag := projectTag(workDir); tag != "" {
		base = "Gogen@" + tag
	}
	label := strings.TrimSpace(ev.Label)
	if label == "" {
		return base
	}
	return base + ": " + label
}

// projectTag is the short project tag for the title ("ttgo" from
// /home/x/ttgo), capped so the session label keeps room on the display's
// first label line (~28 glyphs at the default font). Empty when the
// directory is unknown ("." / "" / a filesystem root).
func projectTag(dir string) string {
	base := dirBase(dir)
	if base == "" {
		return ""
	}
	return truncate(asciiOnly(base), 10)
}

// activityFor is the short human-readable activity for a phase. ASCII only:
// the firmware's built-in fonts have no glyphs above 0x7E, so a "…" would
// render as several garbage glyphs.
func activityFor(ev agent.TurnEvent) string {
	switch ev.Phase {
	case agent.TurnPhaseStarted, agent.TurnPhaseRoundStart, agent.TurnPhaseProgress:
		return "thinking..."
	case agent.TurnPhaseTool:
		if ev.Tool != "" {
			return ev.Tool
		}
		return "tool"
	case agent.TurnPhaseCompacting:
		return "compacting..."
	case agent.TurnPhaseCondensed:
		return "condensed"
	case agent.TurnPhaseStalled:
		return "waiting..."
	case agent.TurnPhaseRetry:
		if ev.Detail != "" {
			return "retrying: " + oneLine(ev.Detail)
		}
		return "retrying"
	}
	return "running"
}

// appendElapsed appends the turn's elapsed time when it is worth showing
// ("thinking... 12s"). A plain space, not a separator glyph: the display's
// default font is ASCII-only and every glyph counts against the bar label
// budget. The context usage is NOT in the text — the bar's percentage (driven
// by progress) is the context indicator.
func appendElapsed(activity string, d time.Duration) string {
	e := formatElapsed(d)
	if e == "" {
		return activity
	}
	if activity == "" {
		return e
	}
	return activity + " " + e
}

// asciiOnly replaces bytes outside the printable ASCII range with '?'. The
// firmware draws the built-in TFT_eSPI fonts (the default is the GLCD table,
// ASCII 0x20..0x7E); a UTF-8 sequence renders as one garbage glyph per byte.
// Applied to every display-bound string so user/model text can never garble
// the bar.
func asciiOnly(s string) string {
	clean := true
	for i := 0; i < len(s); i++ {
		if s[i] < 0x20 || s[i] > 0x7e {
			clean = false
			break
		}
	}
	if clean {
		return s
	}
	b := []byte(s)
	for i := range b {
		if b[i] < 0x20 || b[i] > 0x7e {
			b[i] = '?'
		}
	}
	return string(b)
}

// oneLine collapses whitespace so multi-line error/retry text fits the
// display's single-line message field.
func oneLine(s string) string {
	return strings.Join(strings.Fields(s), " ")
}

// truncate cuts s to at most n runes (the firmware's limits are byte
// counts; cutting on a rune boundary keeps UTF-8 intact).
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

// dirBase returns the base name of a project directory for display use, or ""
// when the directory is unknown ("", ".", or a filesystem root). Shared by
// projectTag (the bar title) and sourceForDir (the row name) so both reject
// the same non-identifying directories.
func dirBase(dir string) string {
	dir = strings.TrimSpace(dir)
	if dir == "" {
		return ""
	}
	base := filepath.Base(filepath.Clean(dir))
	if base == "." || base == string(filepath.Separator) || base == "" {
		return ""
	}
	return base
}

// clamp01 clamps a ratio into the firmware's bar range (percent can exceed 1
// before auto-compaction runs).
func clamp01(v float64) float64 {
	if v < 0 {
		return 0
	}
	if v > 1 {
		return 1
	}
	return v
}
