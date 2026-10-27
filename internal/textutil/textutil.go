// Package textutil holds the string-shaping helpers shared by the packages
// that render or clip text: the TUI, the LAN status display, the agent's
// transcript notes and the automation CLI all cut text to a rune budget, and
// each had its own copy of the same slice-the-runes dance.
//
// These helpers count RUNES, never display cells. They are for plain text —
// agent-facing notes, JSON, log lines, CLI output — and for row text whose
// caller has already established that a rune budget is safe. Anything that is
// laid out on a terminal row must measure cells instead (see the TUI's
// truncateCells/cutCells): a wide rune (CJK, emoji) is two cells, so a rune
// budget can still overflow a cell budget.
package textutil

// TruncateRunes cuts s to at most maxRunes runes and returns s unchanged when
// it is already that short. A non-positive budget yields "".
//
// The cut is on a rune boundary, so the result is always valid UTF-8 (a byte
// cut can split a multi-byte character and emit a replacement glyph).
func TruncateRunes(s string, maxRunes int) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	return string(r[:maxRunes])
}

// TruncateRunesEllipsis cuts s to at most maxRunes runes INCLUDING marker,
// which is appended only when a cut happened. maxRunes therefore bounds the
// result exactly (CLI columns, fixed-width fields), and a budget smaller than
// the marker degrades to a plain cut rather than a bare marker.
//
// Callers that want the marker to sit beyond the budget do not want this
// function: they are bounding content, not laying out a field (see the agent's
// clipRunes, which keeps n runes of content and appends "...").
func TruncateRunesEllipsis(s string, maxRunes int, marker string) string {
	if maxRunes <= 0 {
		return ""
	}
	r := []rune(s)
	if len(r) <= maxRunes {
		return s
	}
	m := []rune(marker)
	if len(m) >= maxRunes {
		return string(r[:maxRunes])
	}
	return string(r[:maxRunes-len(m)]) + marker
}
