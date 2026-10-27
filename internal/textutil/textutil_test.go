package textutil

import (
	"strings"
	"testing"
	"unicode/utf8"
)

func TestTruncateRunes(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"shorter than budget", "hello", 10, "hello"},
		{"exactly the budget", "hello", 5, "hello"},
		{"cut", "hello world", 5, "hello"},
		{"zero budget", "hello", 0, ""},
		{"negative budget", "hello", -3, ""},
		{"empty input", "", 4, ""},
		// Rune, not byte: a byte cut would split the é and emit invalid UTF-8.
		{"multibyte", "héllo wörld", 5, "héllo"},
		{"wide runes are runes here", "日本語テキスト", 3, "日本語"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateRunes(tc.in, tc.max)
			if got != tc.want {
				t.Fatalf("TruncateRunes(%q, %d) = %q, want %q", tc.in, tc.max, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8: %q", got)
			}
		})
	}
}

func TestTruncateRunesEllipsis(t *testing.T) {
	for _, tc := range []struct {
		name   string
		in     string
		max    int
		marker string
		want   string
	}{
		{"shorter than budget", "hello", 10, "…", "hello"},
		{"exactly the budget", "hello", 5, "…", "hello"},
		// The marker counts toward the budget: 5 runes total.
		{"cut with marker", "hello world", 5, "…", "hell…"},
		{"three-rune marker", "hello world", 6, "...", "hel..."},
		{"marker longer than the budget degrades", "hello world", 2, "...", "he"},
		{"zero budget", "hello", 0, "…", ""},
		{"multibyte cut", "héllo wörld", 4, "…", "hél…"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := TruncateRunesEllipsis(tc.in, tc.max, tc.marker)
			if got != tc.want {
				t.Fatalf("TruncateRunesEllipsis(%q, %d, %q) = %q, want %q", tc.in, tc.max, tc.marker, got, tc.want)
			}
			if !utf8.ValidString(got) {
				t.Fatalf("result is not valid UTF-8: %q", got)
			}
			if n := utf8.RuneCountInString(got); n > tc.max && tc.max > 0 {
				t.Fatalf("result is %d runes, want at most %d", n, tc.max)
			}
		})
	}
}

// TestTruncateRunesEllipsisIsStable pins the property the CLI columns rely on:
// truncating an already-truncated string is a no-op.
func TestTruncateRunesEllipsisIsStable(t *testing.T) {
	in := strings.Repeat("ab", 50)
	once := TruncateRunesEllipsis(in, 20, "…")
	if twice := TruncateRunesEllipsis(once, 20, "…"); twice != once {
		t.Fatalf("not stable: %q -> %q", once, twice)
	}
}
