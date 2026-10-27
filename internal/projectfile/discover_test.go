package projectfile

import (
	"path/filepath"
	"runtime"
	"testing"
)

// TestGlobalDirsUseTheHomeDirPolicy pins D17: both global directories resolve
// their base through HomeDir(), so there is exactly one answer to "where is
// home, and what happens when it cannot be determined".
//
// The two call sites used to ignore the os.UserHomeDir error, which turned an
// unresolvable home into an EMPTY base — filepath.Join then produced the same
// relative ".config/gogen" path by accident, with nothing documenting that the
// global config could land in the working directory. HomeDir()'s explicit "."
// makes it a rule the two dirs share (and that a future change cannot silently
// break for one of them).
func TestGlobalDirsUseTheHomeDirPolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("os.UserHomeDir reads USERPROFILE on Windows, so HOME cannot force the fallback")
	}
	t.Setenv("HOME", "")
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("XDG_DATA_HOME", "")

	// One policy: the fallback is HomeDir's ".", not "".
	if got := HomeDir(); got != "." {
		t.Fatalf("HomeDir with no HOME = %q, want %q", got, ".")
	}
	if got, want := GlobalConfigDir(), filepath.Join(HomeDir(), ".config", "gogen"); got != want {
		t.Fatalf("GlobalConfigDir = %q, want %q", got, want)
	}
	if got, want := GlobalDataDir(), filepath.Join(HomeDir(), ".local", "share", "gogen"); got != want {
		t.Fatalf("GlobalDataDir = %q, want %q", got, want)
	}
	// The derived paths follow the same base (no second policy hiding below).
	if got, want := GlobalSessionDir(), filepath.Join(GlobalDataDir(), "sessions"); got != want {
		t.Fatalf("GlobalSessionDir = %q, want %q", got, want)
	}
}
