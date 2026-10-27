package treesitter

import "testing"

// TestLineTextAtOffset pins the offset-based line lookup that replaced a
// whole-file split per reference (O(file) per hit → O(line)).
func TestLineTextAtOffset(t *testing.T) {
	content := []byte("first line\n  second line  \nthird")
	cases := []struct {
		name string
		off  int
		want string
	}{
		{"start of file", 0, "first line"},
		{"middle of first line", 3, "first line"},
		{"start of second line", 11, "second line"},
		{"middle of second line", 20, "second line"},
		{"newline ending the second line", 26, "second line"},
		{"start of last line", 27, "third"},
		{"end of file", len(content), "third"},
		{"past the end", len(content) + 10, "third"},
		{"negative", -5, "first line"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := lineTextAtOffset(content, tc.off); got != tc.want {
				t.Fatalf("lineTextAtOffset(%d) = %q, want %q", tc.off, got, tc.want)
			}
		})
	}
}

// TestAllowedLangsMemoFollowsConfig pins the memo: the parse is cached per raw
// value, so a later environment/config change must still be observed (a
// sync.Once cache would have frozen the first value for the process).
func TestAllowedLangsMemoFollowsConfig(t *testing.T) {
	t.Setenv("GOGEN_TREESITTER_LANGS", "go")
	first := allowedLangs()
	if _, ok := first["go"]; !ok || len(first) != 1 {
		t.Fatalf("allowedLangs() = %v, want {go}", first)
	}
	// Same value: the SAME map is returned (this is the memo).
	if again := allowedLangs(); len(again) != len(first) {
		t.Fatalf("second call = %v", again)
	}
	t.Setenv("GOGEN_TREESITTER_LANGS", "go,json")
	second := allowedLangs()
	if _, ok := second["json"]; !ok {
		t.Fatalf("allowedLangs() after the env change = %v, want json included", second)
	}
	t.Setenv("GOGEN_TREESITTER_LANGS", "")
	if got := allowedLangs(); got != nil {
		t.Fatalf("allowedLangs() with no allowlist = %v, want nil", got)
	}
}
