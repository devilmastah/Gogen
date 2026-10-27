//go:build unix

package pty

import (
	"bytes"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestSignalIntAndTstpWriteControlBytes pins the delivery MECHANISM for the
// keystroke-generated signals: ^C/^Z are written to the pty master so the
// terminal driver raises them in the FOREGROUND process group. Signalling the
// shell's own group instead reaches no job when the shell has job control (the
// job is in its own group), and whether the shell forwards the signal is
// shell-specific luck.
func TestSignalIntAndTstpWriteControlBytes(t *testing.T) {
	cases := []struct {
		name string
		want byte
	}{
		{"SIGINT", 0x03},
		{"SIGTSTP", 0x1a},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			s := &Session{handle: nopWriteCloser{&buf}, done: make(chan struct{})}
			if err := s.Signal(tc.name); err != nil {
				t.Fatalf("Signal(%s): %v", tc.name, err)
			}
			if got := buf.Bytes(); len(got) != 1 || got[0] != tc.want {
				t.Fatalf("wrote %v, want [%#x]", got, tc.want)
			}
		})
	}
	// A process-group signal is NOT a control byte (it must not become the
	// NUL byte the zero value would write).
	if _, ok := controlByteFor("SIGTERM"); ok {
		t.Fatal("SIGTERM must take the process-group path")
	}
}

// nopWriteCloser is a pty-handle stand-in that records writes and never
// blocks on reads.
type nopWriteCloser struct{ *bytes.Buffer }

func (nopWriteCloser) Read([]byte) (int, error) { return 0, io.EOF }
func (nopWriteCloser) Close() error             { return nil }

// TestSignalSigintReachesForegroundJob is the end-to-end shape: a shell with
// job control must come back to the prompt after SIGINT so the next command
// runs. It passes on shells that forward the signal too, so the mechanism
// itself is pinned by TestSignalIntAndTstpWriteControlBytes.
func TestSignalSigintReachesForegroundJob(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed; the job-control shape this pins is bash-specific")
	}
	s, err := Start(Options{
		Shell: bash,
		Args:  []string{"--norc", "--noprofile", "-i"},
		Env:   []string{"TERM=dumb", "PS1=ready> "},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer s.Close()

	// A foreground job that would run for 30s if the signal never arrived.
	if _, err := s.Send("sleep 30", true, 500*time.Millisecond); err != nil {
		t.Fatalf("start sleep: %v", err)
	}
	if err := s.Signal("SIGINT"); err != nil {
		t.Fatalf("signal: %v", err)
	}
	// The shell only runs this once the foreground job is gone.
	res, err := s.Send("echo urgent-$?", true, 5*time.Second)
	if err != nil {
		t.Fatalf("echo after SIGINT: %v", err)
	}
	if !strings.Contains(res.Output, "urgent-") {
		t.Fatalf("command after SIGINT never ran (job survived the signal): %q", res.Output)
	}
}

// TestCloseHangsUpBackgroundJobs pins the Close ordering: SIGHUP first so the
// shell can forward the hangup to its jobs, SIGKILL only afterwards. Killing
// the shell outright left every `&` job running as an orphan — with job
// control the job is in its OWN process group, so the group SIGKILL aimed at
// the shell never touches it.
func TestCloseHangsUpBackgroundJobs(t *testing.T) {
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed; job control (separate job process groups) is what this pins")
	}
	s, err := Start(Options{
		Shell: bash,
		Args:  []string{"--norc", "--noprofile", "-i"},
		Env:   []string{"TERM=dumb", "PS1=ready> "},
	})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	res, err := s.Send("sleep 30 & echo bgpid=$!", true, 500*time.Millisecond)
	if err != nil {
		_ = s.Close()
		t.Fatalf("start background job: %v", err)
	}
	pid := pidAfter(res.Output, "bgpid=")
	if pid <= 1 {
		_ = s.Close()
		t.Fatalf("no background pid in output %q", res.Output)
	}
	// Widen the hangup window for this test: a starved bash must still get the
	// chance to forward SIGHUP to the job (the production default is 1s, and
	// this test asserts the mechanism, not the timing).
	prevGrace := closeHangupGrace
	closeHangupGrace = 5 * time.Second
	t.Cleanup(func() { closeHangupGrace = prevGrace })
	if err := s.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	gone := false
	for i := 0; i < 100; i++ {
		if syscall.Kill(pid, 0) != nil {
			gone = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !gone {
		_ = syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("background job %d survived Close (orphaned)", pid)
	}
}

// pidAfter returns the decimal number following marker in s, or 0.
func pidAfter(s, marker string) int {
	i := strings.LastIndex(s, marker)
	if i < 0 {
		return 0
	}
	rest := s[i+len(marker):]
	end := 0
	for end < len(rest) && rest[end] >= '0' && rest[end] <= '9' {
		end++
	}
	if end == 0 {
		return 0
	}
	n, err := strconv.Atoi(rest[:end])
	if err != nil {
		return 0
	}
	return n
}
