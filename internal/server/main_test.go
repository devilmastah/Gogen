package server

import (
	"os"
	"path/filepath"
	"testing"
)

// TestMain pins a deterministic, config-free shell for the whole package.
//
// Any test that exercises the user's interactive shell spawns it through
// spawnUserTerminal → openUserPTY, which reads $SHELL (or $COMSPEC on
// Windows). A shell that materializes its config directory on startup — fish
// creates $XDG_CONFIG_HOME/fish even for a non-interactive launch — writes
// into the tests' t.TempDir() asynchronously and races its RemoveAll, failing
// cleanup with "directory not empty". Pinning /bin/sh (which reads/writes no
// config for a non-interactive start) makes the spawned terminal
// side-effect-free. Tests that already exercise the terminal pin /bin/sh
// themselves; doing it once here covers every WS test.
func TestMain(m *testing.M) {
	if sh := os.Getenv("SHELL"); sh == "" || filepath.Base(sh) != "sh" {
		_ = os.Setenv("SHELL", "/bin/sh")
	}
	os.Exit(m.Run())
}
