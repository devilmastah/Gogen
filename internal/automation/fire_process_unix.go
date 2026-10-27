//go:build unix

package automation

import (
	"os/exec"
	"syscall"
	"time"
)

// fireKillGrace is how long a timed-out run gets between SIGTERM and SIGKILL.
// Long enough for the child to persist its final turn state, short enough that
// a hung run does not hold the automation slot.
const fireKillGrace = 3 * time.Second

// setFireProcessGroup puts the child in its own process group, so the timeout
// can signal the WHOLE tree it created (shell jobs, tool subprocesses) instead
// of leaving them running as orphans.
func setFireProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killFireProcess terminates the child's process group: SIGTERM first so the
// run can clean up, then SIGKILL after the grace period. The previous
// cmd.Process.Kill() hit only the direct child (SIGKILL, no cleanup).
func killFireProcess(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pid := cmd.Process.Pid
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.Now().Add(fireKillGrace)
	for time.Now().Before(deadline) {
		// Kill(pid, 0) succeeds while the process still exists (a zombie counts
		// until the wait goroutine reaps it, which happens promptly).
		if err := syscall.Kill(pid, 0); err != nil {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
}
