package agent

import (
	"context"
	"fmt"
	"io"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"gogen/internal/contextmgr"
	"gogen/internal/procutil"
	"gogen/internal/randhex"
)

// BackgroundJob is one shell command running outside a turn. The turn that
// started it (execute_command background=true) returns immediately with the
// job's id; the command keeps running until it exits on its own, is cancelled
// (background_job action=cancel), or its owning session is closed (Agent.Close kills
// every job, so a closed web pane or a TUI quit never orphans a process).
// Output is retained as a bounded tail so a long-running job cannot grow
// memory without bound while it is polled. Finished jobs stay registered
// for a retention window (defaultBackgroundJobRetain) so background_job
// action=status can report their exit code and output after completion, then
// the reaper removes them; a finished-job cap
// (defaultMaxFinishedBackgroundJobs) bounds how many can accumulate within
// that window. Close removes everything.
type BackgroundJob struct {
	ID      string
	Command string
	Start   time.Time

	// cancelled records an explicit CancelBackgroundJob/Close kill. A group
	// kill makes Wait return an *exec.ExitError (signaled), not
	// context.Canceled, so the flag is what distinguishes cancel from failure.
	cancelled atomic.Bool

	done   chan struct{} // closed when the process exits
	cancel context.CancelFunc
	mu     sync.Mutex
	output *boundedOutputWriter
	// unread holds output produced since the last action=input drain, so
	// input can return the delta since the previous read instead of the
	// full tail. Fed by the same chunks as output (both buffers are written
	// under their own locks); drained by BackgroundJobInput. action=status
	// keeps showing the full tail.
	unread *boundedOutputWriter
	// stdin is the job's stdin pipe, created by cmd.StdinPipe before the
	// process starts. stdinMu serializes writes so logical lines stay
	// atomic; it also guards the pipe against being written while the wait
	// goroutine's cmd.Wait is closing it.
	stdin   io.WriteCloser
	stdinMu sync.Mutex
	// stdinBusy is closed when an ABANDONED write (its caller cancelled while
	// the pipe was blocked) finally returns. The next input call waits on it so
	// two writers never interleave on one stdin pipe.
	stdinBusy chan struct{}
	// finishedAt is when the process exited; set under mu before done is
	// closed. The finished-job cap uses it to reap the oldest finished jobs
	// first.
	finishedAt time.Time
	// reaper is the retention timer that removes this finished job from the
	// registry after the retention window. Stopped by Close and by cap
	// reaping so a reaped job cannot keep the Agent alive. Guarded by mu.
	reaper *time.Timer
	// exitErr is nil on success, context.Canceled when the job was cancelled
	// (or the session closed), an *exec.ExitError for a non-zero exit, or
	// another error for launch/IO failures.
	exitErr error
}

// newBackgroundJobID returns a random hex job id.
func newBackgroundJobID() string {
	return randhex.ID(8, "job-")
}

// StartBackgroundCommand validates the command against the same command
// guard and sandbox as execute_command, launches it detached from the turn,
// and returns its job id. The job runs with no turn-bound timeout: it is
// cancelled via CancelBackgroundJob or killed when the session closes
// (Agent.Close). Finished jobs stay registered for a bounded retention
// window, then the reaper removes them.
//
// ctx is the starting tool call's context: when it carries a live-output
// sink and end callback (the web UI's terminal streaming), every output
// chunk is forwarded to the sink for the job's WHOLE lifetime — the job
// outlives the turn that started it — and end fires when the process
// exits so the UI closes the terminal with the real exit status. The
// empty-chunk sink call right after launch opens the terminal (and the
// chat card's command echo) before the tool result is reported.
func (a *Agent) StartBackgroundCommand(ctx context.Context, command string) (string, error) {
	if a.Executor == nil {
		return "", fmt.Errorf("no executor configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	sink := ToolOutputFromContext(ctx)
	end := ToolOutputEndFromContext(ctx)
	// Detached from the turn (a background job outlives it) but carrying the
	// turn's VALUES: the command approver lives in the context, and a job whose
	// context dropped it could never ask the user about a blocked command — the
	// guard failed closed and refused commands the foreground path would have
	// offered for approval. WithoutCancel keeps the values and drops the
	// cancellation/deadline; the job gets its own cancel below.
	jobCtx, cancel := context.WithCancel(context.WithoutCancel(ctx))
	job := &BackgroundJob{
		ID:      newBackgroundJobID(),
		Command: command,
		Start:   time.Now(),
		done:    make(chan struct{}),
		cancel:  cancel,
		output:  newBoundedOutputWriter(defaultBackgroundOutputCap),
		unread:  newBoundedOutputWriter(defaultBackgroundUnreadCap),
	}
	a.bgMu.Lock()
	if a.bgClosed {
		// Close already swept its snapshot of the jobs and latched the door:
		// a job registered now would never be killed, and its reaper would keep
		// the closed agent reachable for 30 minutes.
		a.bgMu.Unlock()
		cancel()
		return "", fmt.Errorf("session is closing; cannot start a background command")
	}
	if a.bgJobs == nil {
		a.bgJobs = make(map[string]*BackgroundJob)
	}
	a.bgJobs[job.ID] = job
	a.bgMu.Unlock()

	// Launch through the platform runner: sh -c with an OS stdin pipe on
	// Unix (buffered, EPIPE semantics), the embedded POSIX interpreter with
	// an in-memory stdin pipe on Windows (no external sh required). Every
	// output chunk lands in both the tail and the unread delta buffer; the
	// MultiWriter preserves per-stream write order, and stdout/stderr
	// interleaving follows the child's write order. A live-output sink
	// rides along as a third writer: it is never capped (like the
	// foreground path), so the UI sees the full log while the tail stays
	// bounded.
	writers := []io.Writer{job.output, job.unread}
	if sink != nil {
		writers = append(writers, sinkWriter{command: command, sink: sink})
	}
	combined := io.MultiWriter(writers...)
	stdin, wait, err := a.Executor.launchBackground(jobCtx, command, combined, combined)
	if err != nil {
		a.bgMu.Lock()
		delete(a.bgJobs, job.ID)
		a.bgMu.Unlock()
		cancel()
		return "", err
	}
	job.stdin = stdin
	// Open the UI's live terminal now (before the tool result is
	// reported): the empty chunk carries no output, only the command
	// echo. A first real chunk that wins the race opens it instead —
	// the server-side handler is idempotent per tool call id.
	if sink != nil {
		sink(command, "")
	}
	go func() {
		waitErr := wait()
		job.mu.Lock()
		job.exitErr = waitErr
		job.finishedAt = time.Now()
		job.mu.Unlock()
		close(job.done)
		// The output stream is over: close the UI's terminal with the
		// real exit status. May fire long after the starting turn ended
		// (the sink/end closures outlive it); a cancelled job reports
		// failure.
		if end != nil {
			end(waitErr == nil)
		}
		a.onJobFinished(job)
	}()
	return job.ID, nil
}

// sinkWriter forwards each background-job output chunk to a ToolOutputSink.
// It is shared by the job's stdout and stderr MultiWriters, so per-stream
// write order is preserved by exec's pipe-copy goroutines exactly as for
// the tail buffers.
type sinkWriter struct {
	command string
	sink    ToolOutputSink
}

func (w sinkWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.sink(w.command, string(p))
	return len(p), nil
}

// maxBackgroundInputBytes caps a single action=input write. The kernel pipe
// buffer absorbs roughly 64 KB before a writer blocks, so a cap this small
// bounds how long a write can wait behind a job that never reads stdin: the
// write still blocks once the buffer fills (until the job reads or exits —
// documented in the tool description), but never with more than a few
// outstanding writes.
const maxBackgroundInputBytes = 16 * 1024

// maxShowBytes caps the output tail a background_job status result embeds,
// so a chatty job cannot bloat the transcript. The tail cut is rune-safe
// (contextmgr.RuneSafeTailStart), so the shown tail can be a few bytes
// smaller than this cap.
const maxShowBytes = 8 * 1024

// defaultBackgroundUnreadCap bounds the per-job output delta retained for
// action=input, mirroring the output tail cap so a long-running job cannot
// grow memory without bound.
const defaultBackgroundUnreadCap = 64 * 1024

// BackgroundJobInput writes text to a running background job's stdin and
// returns the output produced since the previous read (the delta), so REPL
// loops can poll cheaply without re-reading the whole tail. The write is
// serialized per job. Input is not a shell command: it goes to the process's
// stdin, so the command guard deliberately does not apply.
func (a *Agent) BackgroundJobInput(ctx context.Context, jobID, input string, appendNewline bool) (string, error) {
	if a.terminalJob(jobID) != nil {
		return "", fmt.Errorf("job %s is a background terminal send; send more input with the terminal tool (action=send) or interrupt it with action=signal", jobID)
	}
	job := a.backgroundJob(jobID)
	if job == nil {
		return "", fmt.Errorf("unknown background job %q (jobs are scoped to this session; the session may have been closed)", jobID)
	}
	if len(input) > maxBackgroundInputBytes {
		return "", fmt.Errorf("input is %d bytes; background_job input accepts at most %d bytes per call", len(input), maxBackgroundInputBytes)
	}
	select {
	case <-job.done:
		return "", fmt.Errorf("background job %s already finished", jobID)
	default:
	}
	if job.stdin == nil {
		return "", fmt.Errorf("background job %s has no stdin pipe", jobID)
	}

	payload := input
	if appendNewline {
		payload += "\n"
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return "", err
	}
	job.stdinMu.Lock()
	defer job.stdinMu.Unlock()
	// A previous call may have been cancelled while its write was still
	// blocked in the pipe (the child is not reading). Wait for that write
	// before starting a new one so the pipe never sees two writers.
	if job.stdinBusy != nil {
		select {
		case <-job.stdinBusy:
			job.stdinBusy = nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}
	// The write runs in its own goroutine so a cancellation (Esc) can return
	// instead of hanging until the child happens to read: a job that never
	// reads stdin fills the pipe (~64 KB) and Write blocks forever.
	type writeResult struct {
		n   int
		err error
	}
	done := make(chan writeResult, 1)
	released := make(chan struct{})
	go func() {
		defer close(released)
		n, werr := job.stdin.Write([]byte(payload))
		done <- writeResult{n: n, err: werr}
	}()
	var n int
	var err error
	select {
	case res := <-done:
		n, err = res.n, res.err
	case <-job.done:
		return "", fmt.Errorf("background job %s exited while writing to its stdin", jobID)
	case <-ctx.Done():
		job.stdinBusy = released
		return "", ctx.Err()
	}
	if err == nil && n < len(payload) {
		err = io.ErrShortWrite
	}
	if err != nil {
		if isStdinClosedErr(err) {
			return "", fmt.Errorf("background job %s closed its stdin (the process exited)", jobID)
		}
		return "", fmt.Errorf("writing to background job %s stdin: %w", jobID, err)
	}

	delta := job.unread.drain()
	if delta == "" {
		return fmt.Sprintf("Sent %d bytes to job %s stdin. No new output since the last read.", len(payload), jobID), nil
	}
	return fmt.Sprintf("Sent %d bytes to job %s stdin.\nOutput:\n%s", len(payload), jobID, delta), nil
}

// backgroundJob returns the live job for id, or nil.
func (a *Agent) backgroundJob(jobID string) *BackgroundJob {
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	return a.bgJobs[jobID]
}

// BackgroundJobStatus reports the state of a background job: whether it is
// still running, its exit code when finished, and the tail of its output.
// Finished jobs stay registered for a bounded retention window, so the agent
// can poll for the result after completion. "Unknown job" only happens for
// ids that never existed, were already reaped, or whose session was closed.
func (a *Agent) BackgroundJobStatus(jobID string) (string, error) {
	if tj := a.terminalJob(jobID); tj != nil {
		return a.terminalJobStatus(jobID)
	}
	job := a.backgroundJob(jobID)
	if job == nil {
		return "", fmt.Errorf("unknown background job %q (jobs are scoped to this session; the session may have been closed)", jobID)
	}
	elapsed := time.Since(job.Start).Round(time.Millisecond)

	state, exitCode := jobState(job)
	switch state {
	case "RUNNING":
		return fmt.Sprintf("Job %s is RUNNING (%s elapsed).\nCommand: %s\n%s", job.ID, elapsed, job.Command, formatJobOutput(job, true)), nil
	case "CANCELLED":
		return fmt.Sprintf("Job %s was CANCELLED after %s.\nCommand: %s\n%s", job.ID, elapsed, job.Command, formatJobOutput(job, false)), nil
	}
	return fmt.Sprintf("Job %s FINISHED in %s with exit code %d.\nCommand: %s\n%s", job.ID, elapsed, exitCode, job.Command, formatJobOutput(job, false)), nil
}

// backgroundWaitDefaultTimeout is how long action=wait blocks for a job to
// exit when the caller does not pass timeout_ms.
const backgroundWaitDefaultTimeout = 60 * time.Second

// backgroundWaitMaxTimeout caps a caller-supplied wait so one tool call can
// never park the model loop for an unbounded wall time. A job that outlives
// the cap keeps running and can be waited on again.
const backgroundWaitMaxTimeout = 10 * time.Minute

// jobDone returns the channel closed when jobID exits, reporting false when
// no live job carries the id. Shell jobs and background terminal sends share
// the job-id space (status and cancel dispatch on it), so a wait must resolve
// both registries.
func (a *Agent) jobDone(jobID string) (<-chan struct{}, bool) {
	if tj := a.terminalJob(jobID); tj != nil {
		return tj.done, true
	}
	if job := a.backgroundJob(jobID); job != nil {
		return job.done, true
	}
	return nil, false
}

// BackgroundJobWait blocks until the job exits or timeout elapses, then
// reports the same status action=status would. A timeout is deliberately not
// an error: the report says the job is still running, so the caller waits
// again or cancels. ctx aborts the wait (a cancelled turn must not park the
// loop); the job itself keeps running either way. Waiting never consumes the
// job's result — the retention window still holds the output and exit code
// for a later status.
func (a *Agent) BackgroundJobWait(ctx context.Context, jobID string, timeout time.Duration) (string, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	done, ok := a.jobDone(jobID)
	if !ok {
		return "", fmt.Errorf("unknown background job %q (jobs are scoped to this session; the session may have been closed)", jobID)
	}
	// A nil timer channel blocks forever, so timeout <= 0 waits until the
	// job exits or ctx is cancelled.
	var timer <-chan time.Time
	if timeout > 0 {
		t := time.NewTimer(timeout)
		defer t.Stop()
		timer = t.C
	}
	timedOut := false
	select {
	case <-done:
	case <-timer:
		timedOut = true
	case <-ctx.Done():
		return "", fmt.Errorf("waiting for background job %s interrupted: %w (the job keeps running; poll it with action=status or cancel it with action=cancel)", jobID, ctx.Err())
	}
	status, err := a.BackgroundJobStatus(jobID)
	if err != nil {
		return "", err
	}
	if timedOut {
		return fmt.Sprintf("%s\nWait timed out after %s; the job is still running. Wait again (action=wait) or cancel it (action=cancel).", status, timeout), nil
	}
	return status, nil
}

// formatJobOutput returns the job's retained output tail, trimmed and
// labelled, or a note when the job produced none. running distinguishes the
// note text: "so far" only makes sense while the job can still produce
// output.
func formatJobOutput(job *BackgroundJob, running bool) string {
	out := job.output.String()
	if out == "" {
		if running {
			return "Output: (none so far)"
		}
		return "Output: (none)"
	}
	if len(out) > maxShowBytes {
		// Rune-safe tail cut: slicing at len(out)-maxShowBytes could split
		// a multi-byte UTF-8 rune at the start of the shown tail and inject
		// invalid UTF-8 into the tool result — the same bug class the head
		// cuts fix. Back off to a rune boundary; the reported byte count is
		// the actually-shown length, which can be a few bytes smaller.
		tail := out[contextmgr.RuneSafeTailStartString(out, maxShowBytes):]
		return fmt.Sprintf("Output (last %d bytes of %d):\n%s", len(tail), len(out), tail)
	}
	return "Output:\n" + out
}

// maxListCommandLen caps the command (or terminal input) echoed per job in
// the list view, so a job started with a very long command cannot bloat the
// transcript.
const maxListCommandLen = 80

// BackgroundJobsList renders every job owned by this session — shell commands
// started with execute_command background=true and background terminal sends
// — oldest first, with state, elapsed time, exit code and a truncated
// command. It answers "what is still running?" in one call instead of a poll
// per known id, which also lets a resumed turn recover job ids it no longer
// holds. A reaped job is gone for good (retention window elapsed,
// finished-job cap, or session close).
func (a *Agent) BackgroundJobsList() string {
	a.bgMu.Lock()
	jobs := make([]*BackgroundJob, 0, len(a.bgJobs))
	for _, job := range a.bgJobs {
		jobs = append(jobs, job)
	}
	a.bgMu.Unlock()
	a.termMu.Lock()
	termJobs := make([]*terminalJob, 0, len(a.termJobs))
	for _, job := range a.termJobs {
		termJobs = append(termJobs, job)
	}
	a.termMu.Unlock()

	type entry struct {
		start time.Time
		line  string
	}
	entries := make([]entry, 0, len(jobs)+len(termJobs))
	for _, job := range jobs {
		entries = append(entries, entry{
			start: job.Start,
			line: fmt.Sprintf("- %s %s (%s): %s", job.ID, backgroundJobState(job),
				time.Since(job.Start).Round(time.Millisecond),
				contextmgr.Truncate(job.Command, maxListCommandLen, contextmgr.TruncateOptions{Marker: "…"})),
		})
	}
	for _, job := range termJobs {
		entries = append(entries, entry{
			start: job.Start,
			line: fmt.Sprintf("- %s %s (%s, terminal %s): %s", job.ID, terminalJobState(job),
				time.Since(job.Start).Round(time.Millisecond), job.SessionID,
				contextmgr.Truncate(job.Text, maxListCommandLen, contextmgr.TruncateOptions{Marker: "…"})),
		})
	}
	if len(entries) == 0 {
		return "No background jobs in this session."
	}
	// Oldest first: the long-running job a caller is looking for is the one
	// that started earliest.
	slices.SortFunc(entries, func(a, b entry) int { return a.start.Compare(b.start) })
	var b strings.Builder
	fmt.Fprintf(&b, "Background jobs (%d, oldest first):", len(entries))
	for _, e := range entries {
		b.WriteString("\n")
		b.WriteString(e.line)
	}
	return b.String()
}

// jobState reports a shell job's lifecycle state and its exit code at one
// instant: "RUNNING", "CANCELLED", or "FINISHED" (with the code; -1 for a
// signal kill or an unusual failure). The status reply and the list view both
// render from it, so they can never disagree about whether a job finished or
// about its code — they used to read the done channel, the cancel flag and
// exitErr independently.
func jobState(job *BackgroundJob) (state string, exitCode int) {
	select {
	case <-job.done:
	default:
		return "RUNNING", 0
	}
	if job.cancelled.Load() {
		return "CANCELLED", 0
	}
	job.mu.Lock()
	err := job.exitErr
	job.mu.Unlock()
	return "FINISHED", procutil.ExitCode(err)
}

// backgroundJobState renders a shell job's one-word state for the list view:
// RUNNING, CANCELLED, or FINISHED with the exit code (-1 for a signal kill
// or an unusual failure).
func backgroundJobState(job *BackgroundJob) string {
	state, code := jobState(job)
	if state == "FINISHED" {
		return fmt.Sprintf("FINISHED exit %d", code)
	}
	return state
}

// CancelBackgroundJob cancels a running background job, killing its process
// group (the same group-kill execute_command cancellation uses).
func (a *Agent) CancelBackgroundJob(jobID string) (string, error) {
	if tj := a.terminalJob(jobID); tj != nil {
		return a.cancelTerminalJob(jobID)
	}
	job := a.backgroundJob(jobID)
	if job == nil {
		return "", fmt.Errorf("unknown background job %q (it may have finished and been reaped)", jobID)
	}
	select {
	case <-job.done:
		return "", fmt.Errorf("background job %s already finished", jobID)
	default:
	}
	job.cancelled.Store(true)
	job.cancel()
	return fmt.Sprintf("Cancelled background job %s (command: %s).", jobID, job.Command), nil
}

// Close kills every background job of this session. Called when a session is
// closed (web pane close / session delete / shutdown) and at process exit, so
// no command is ever orphaned by a session that no longer exists. Idempotent.
// Retention timers are stopped so a reaped job cannot keep the (closing)
// Agent alive; the wait goroutine of a cancelled job finds an empty registry
// afterwards and does not arm a new timer. It is a real end-of-session: an
// eviction that only recycles a session's MEMORY (a web pane switch, a page
// refresh, registry-cap pressure) must not call it — that is what
// HasBackgroundWork is for.
func (a *Agent) Close() {
	// Latch closed BEFORE tearing anything down, so a job's or a terminal
	// send's wait goroutine unwinding mid-teardown cannot fire the
	// background-idle hook into a host that is already closing this session
	// (it would re-run the host's teardown decision). The latch is only ever
	// set, never cleared: Close is a one-way door for this Agent.
	a.bgMu.Lock()
	a.bgClosed = true
	a.bgMu.Unlock()
	// Close every owner-scoped persistent terminal and its background jobs
	// alongside the shell jobs: a closed session must not orphan a PTY.
	a.closeTerminals()
	a.bgMu.Lock()
	jobs := a.bgJobs
	a.bgJobs = nil
	for _, job := range jobs {
		job.cancelled.Store(true)
		job.cancel()
		job.mu.Lock()
		if job.reaper != nil {
			job.reaper.Stop()
		}
		job.mu.Unlock()
	}
	a.bgMu.Unlock()
}

// HasBackgroundWork reports whether this session still owns background work
// an eviction would destroy: a running shell job (execute_command
// background=true), a running background terminal send, a live persistent
// terminal (PTY), or the persistent shell behind bash_persistent.
//
// A session that owns background work is NOT idle, even with no client
// attached: hosts use this to keep such a session resident instead of
// treating "nobody is watching" as "safe to tear down", because
// Agent.Close — the eviction tail — kills exactly this work. Leaf locks
// only (bgMu, termMu); it never blocks, so it is safe on an eviction path.
func (a *Agent) HasBackgroundWork() bool {
	a.bgMu.Lock()
	running := false
	for _, job := range a.bgJobs {
		select {
		case <-job.done:
			continue // finished: still pollable for the retention window, not work
		default:
			running = true
		}
	}
	a.bgMu.Unlock()
	return running || a.hasTerminalWork()
}

// SetBackgroundIdleHook installs a callback fired when this session's LAST
// background work finishes (see HasBackgroundWork) and none has started
// since. Hosts use it to re-run a decision they deferred while the work was
// running — notably a web session runtime that refused to evict itself
// because it still owned a job: this is what evicts it once the job drains,
// so deferring the eviction cannot leak the runtime forever. nil detaches.
// Never fires after Close.
func (a *Agent) SetBackgroundIdleHook(h func()) {
	a.bgIdleHookMu.Lock()
	a.bgIdleHook = h
	a.bgIdleHookMu.Unlock()
}

// maybeFireBackgroundIdle fires the background-idle hook when no background
// work remains. Called from the wait goroutines that close a job's / a
// terminal send's done channel and from the terminal close/reset paths, so
// it must not block: the hook is invoked with no lock held.
//
// The drained check is inherently racy against a job starting concurrently
// (the hook may fire just as new work appears); that is harmless — the
// hook's own decision must tolerate it, exactly like the eviction TryLock
// races it replaces. The next drain fires again.
func (a *Agent) maybeFireBackgroundIdle() {
	a.bgIdleHookMu.RLock()
	h := a.bgIdleHook
	a.bgIdleHookMu.RUnlock()
	if h == nil {
		return
	}
	a.bgMu.Lock()
	closed := a.bgClosed
	a.bgMu.Unlock()
	if closed || a.HasBackgroundWork() {
		return
	}
	h()
}

const defaultBackgroundOutputCap = 256 * 1024

// defaultBackgroundJobRetain is how long a finished background job stays
// registered after it exits, so background_job_status can still report its
// exit code and output. The window must comfortably outlive a compaction:
// a job started before the middle of the history was summarized is still
// addressable afterwards via background_job (action=list/status), so the
// model can re-correlate it from the summary's executed-call roster. After
// the window the reaper removes it, bounding memory on long-lived sessions
// (each job keeps an outputBuffer tail); defaultMaxFinishedBackgroundJobs
// remains the harder bound. The Agent.bgRetain field overrides it
// (0 = this default).
const defaultBackgroundJobRetain = 30 * time.Minute

// defaultMaxFinishedBackgroundJobs caps how many finished jobs stay
// registered at once: when a job finishes and the cap is exceeded, the
// oldest finished jobs are reaped immediately, so a burst of short-lived
// jobs cannot pile up during the retention window. The Agent.bgMaxFinished
// field overrides it (0 = this default).
const defaultMaxFinishedBackgroundJobs = 32

// maxJobNoticeCommandLen caps the command echoed in a job-completion
// notice: long commands (and any secrets embedded in them) must not be
// dumped into the transcript.
const maxJobNoticeCommandLen = 120

// jobExitSummary renders the one-line job-completion notice: id, truncated
// command, and exit code (-1 for unusual failures such as I/O errors).
// Called from the job's wait goroutine after exitErr is final.
func jobExitSummary(job *BackgroundJob) string {
	code := procutil.ExitCode(job.exitErr)
	// Truncate makes a rune-safe cut: a raw byte slice could split a
	// multi-byte character and emit invalid UTF-8 into the transcript.
	cmd := contextmgr.Truncate(job.Command, maxJobNoticeCommandLen,
		contextmgr.TruncateOptions{Marker: "…"})
	return contextmgr.JobNoticePrefix + fmt.Sprintf("%s (%s) exited with code %d", job.ID, cmd, code)
}

// onJobFinished runs in the job's wait goroutine once the process has
// exited: it enforces the finished-job cap (reaping the oldest finished jobs
// when the cap is exceeded), arms the retention timer that reaps THIS job
// once its result has been pollable long enough, and — when the job finished
// naturally (not cancelled) and a notice hook is installed — fires the
// completion notice so the session is told without polling. Last, it lets the
// host know the session may have drained its background work (a host that
// kept this session resident because of THIS job can now retire it).
func (a *Agent) onJobFinished(job *BackgroundJob) {
	a.enforceFinishedJobCap()
	a.armJobReaper(job)
	if !job.cancelled.Load() {
		a.fireJobNotice(jobExitSummary(job))
	}
	a.maybeFireBackgroundIdle()
}

// enforceFinishedJobCap reaps the oldest finished jobs when more than
// bgMaxFinished jobs have finished, so the registry stays bounded even when
// many short-lived jobs finish inside the retention window. The retained set
// is the newest bgMaxFinished finishers, ordered by finishedAt with Start and
// ID as total-order tiebreaks: the outcome is a function of the finished
// SET alone, never of which wait goroutine happens to enforce last or of the
// registry's map iteration order. (Excluding the calling job — "its caller is
// most likely to poll it next" — made the victims scheduling-dependent: when
// the OLDEST finisher's goroutine ran last, excluding it kept the oldest job
// and reaped newer ones, leaving fewer than the expected oldest jobs reaped
// and even over-reaping when an already-reaped job's goroutine enforced
// late. In the normal case the calling job is the newest finisher and is
// retained anyway.)
func (a *Agent) enforceFinishedJobCap() {
	maxFinished := a.bgMaxFinished
	if maxFinished <= 0 {
		maxFinished = defaultMaxFinishedBackgroundJobs
	}
	type finishedJob struct {
		job *BackgroundJob
		at  time.Time
		// start and id break finishedAt ties deterministically (a coarse
		// clock — Windows ticks at ~15ms — or two processes exiting within
		// the same tick): without them the victim set could differ between
		// two sorts of the same jobs.
		start time.Time
		id    string
	}
	var finished []finishedJob
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	for _, j := range a.bgJobs {
		select {
		case <-j.done:
			j.mu.Lock()
			at := j.finishedAt
			j.mu.Unlock()
			finished = append(finished, finishedJob{job: j, at: at, start: j.Start, id: j.ID})
		default:
		}
	}
	overflow := len(finished) - maxFinished
	if overflow <= 0 {
		return
	}
	slices.SortFunc(finished, func(a, b finishedJob) int {
		if c := a.at.Compare(b.at); c != 0 {
			return c
		}
		if c := a.start.Compare(b.start); c != 0 {
			return c
		}
		return strings.Compare(a.id, b.id)
	})
	for i := 0; i < overflow && i < len(finished); i++ {
		j := finished[i]
		delete(a.bgJobs, j.job.ID)
		j.job.mu.Lock()
		if j.job.reaper != nil {
			j.job.reaper.Stop()
		}
		j.job.mu.Unlock()
	}
}

// armJobReaper arms the retention timer that removes job from the registry
// after the retention window. It is a no-op when the job is no longer
// registered (session closed — Close cleared the registry — or already
// reaped), so a reaper can never hold the Agent alive after Close.
// Lock order: bgMu is held while setting reaper under job.mu, matching
// Close and enforceFinishedJobCap (bgMu -> job.mu, never the reverse).
func (a *Agent) armJobReaper(job *BackgroundJob) {
	retain := a.bgRetain
	if retain <= 0 {
		retain = defaultBackgroundJobRetain
	}
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	if a.bgJobs[job.ID] != job || job.reaper != nil {
		return
	}
	job.reaper = time.AfterFunc(retain, func() {
		a.reapBackgroundJob(job)
	})
}

// reapBackgroundJob removes a finished job from the registry once its
// retention window has elapsed. A job that was already reaped (cap
// enforcement or Close) or that is still running is a no-op.
func (a *Agent) reapBackgroundJob(job *BackgroundJob) {
	a.bgMu.Lock()
	defer a.bgMu.Unlock()
	if a.bgJobs[job.ID] == job {
		delete(a.bgJobs, job.ID)
	}
}

// boundedOutputWriter accumulates a command's combined stdout+stderr but
// keeps only the last max bytes, so a long-running background job cannot
// grow memory without bound. Write may be called concurrently by exec's
// pipe-copy goroutines, so the buffer is mutex-guarded.
type boundedOutputWriter struct {
	outputBuffer
	max int
}

func newBoundedOutputWriter(max int) *boundedOutputWriter {
	return &boundedOutputWriter{max: max}
}

func (w *boundedOutputWriter) Write(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	w.append(p, w.max, nil)
	return len(p), nil
}

func (w *boundedOutputWriter) String() string {
	return w.string()
}

// SetJobNoticeHook installs a callback invoked when a background job
// (execute_command background=true) finishes naturally; it receives a
// one-line summary. Hosts wire it to the message-delivery service so the
// session is notified without polling. nil detaches.
func (a *Agent) SetJobNoticeHook(h func(summary string)) {
	a.jobNoticeHookMu.Lock()
	a.jobNoticeHook = h
	a.jobNoticeHookMu.Unlock()
}

// fireJobNotice invokes the installed job-notice hook, if any.
func (a *Agent) fireJobNotice(summary string) {
	a.jobNoticeHookMu.RLock()
	h := a.jobNoticeHook
	a.jobNoticeHookMu.RUnlock()
	if h != nil {
		h(summary)
	}
}

// backgroundState groups the session-owned background shell jobs (bgMu) and
// the per-session read_image byte budget (sessionImages).
type backgroundState struct {
	// bgMu guards bgJobs: shell commands started with execute_command
	// background=true. Jobs outlive the turn that started them (they are
	// owned by the session, not the turn) and are killed when the session
	// closes (Close) or individually via background_job (action=cancel).
	bgMu   sync.Mutex
	bgJobs map[string]*BackgroundJob
	// bgRetain is how long a finished background job stays registered for
	// status polling before the reaper removes it (0 = the
	// defaultBackgroundJobRetain window). Read only from the job's wait
	// goroutine; tests set it before starting jobs.
	bgRetain time.Duration
	// bgMaxFinished caps how many finished jobs stay registered at once
	// (0 = defaultMaxFinishedBackgroundJobs); when a job finishes over the
	// cap, the oldest finished jobs are reaped immediately.
	bgMaxFinished int

	// sessionImages counts read_image attachments in this session. Atomic:
	// read_image handlers run concurrently in parallel read-only batches,
	// so the claim (reserveImageSlot) is a CAS loop. Enforces the soft
	// per-session cap (maxSessionImages) on image bytes — the most
	// expensive content in context, resent on every API request.
	sessionImages atomic.Int32

	// bgClosed latches Close: once set, the background-idle hook never
	// fires again. Guarded by bgMu, set-only — a closing Agent must not
	// re-enter its host's teardown decision from a job's wait goroutine
	// unwinding mid-Close.
	bgClosed bool

	// bgIdleHookMu guards bgIdleHook, fired when the session's last
	// background work drains (maybeFireBackgroundIdle). Its own lock so the
	// hook is invoked with none of this group's locks held.
	bgIdleHookMu sync.RWMutex
	bgIdleHook   func()
}
