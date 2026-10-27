// Package procutil holds the small process-status helpers shared by the
// packages that run child processes (the agent's background jobs and the
// pseudo-terminal sessions). Both need the same two answers — "what exit code
// did this child produce?" and "did it die from a signal?" — and each used to
// derive them slightly differently.
package procutil

import (
	"errors"
	"os/exec"
)

// UnknownExit is the exit code reported when a child finished without one: a
// signal kill, an I/O error, or a start failure. It is the shell convention
// (128+N is only used when the signal number is known, which exec.ExitError
// already reports through ExitCode).
const UnknownExit = -1

// ExitCode maps a child process's error to a shell-style exit code: 0 for
// err == nil, the process's own code for a normal exit (including the
// signal-derived 128+N that exec reports), and UnknownExit for anything else
// (a kill, a start failure, an I/O error).
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return UnknownExit
}

// WasSignaled reports whether a child was terminated by a signal rather than
// exiting on its own (exec reports that as a negative ExitCode, which ExitCode
// passes through).
func WasSignaled(err error) bool {
	var ee *exec.ExitError
	return errors.As(err, &ee) && ee.ExitCode() < 0
}
