// Package onoff centralizes parsing of the on/off spellings accepted across
// GoGen configuration: config files, environment variables, and the live
// config-WS. It is the single source of truth for which strings mean "on"
// and which mean "off", so the spellings are maintained in one place.
package onoff

import "strings"

// Parse reports whether v is a recognized on/off spelling. Matching is
// case-insensitive and ignores surrounding whitespace. ok is false for
// anything that is not a recognized spelling (including the empty string);
// on is only meaningful when ok is true.
func Parse(v string) (on, ok bool) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "on", "1", "true", "yes":
		return true, true
	case "off", "0", "false", "no":
		return false, true
	}
	return false, false
}

// Enabled reports whether v is a recognized "on" spelling. Unknown values
// (including the empty string) are treated as disabled; use Parse when the
// caller must distinguish "explicitly off" from "unrecognized".
func Enabled(v string) bool {
	on, ok := Parse(v)
	return ok && on
}

// Format renders a boolean as the canonical "on"/"off" spelling accepted by
// Parse. The config, the CLI and the config-WS all emit this form; keeping it
// here is what stops the two copies that had drifted (CLI vs web) from
// disagreeing again.
func Format(on bool) string {
	if on {
		return "on"
	}
	return "off"
}
