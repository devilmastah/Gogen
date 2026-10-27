//go:build windows

package automation

import "os/exec"

// setFireProcessGroup is a no-op on Windows: there is no process group for
// os/exec to join here (taskkill /T is what a Windows tree-kill would need).
func setFireProcessGroup(*exec.Cmd) {}

// killFireProcess terminates the child. The existing Windows behaviour is kept:
// only the direct process is killed.
func killFireProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}
