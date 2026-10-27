package agent

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
)

// TestGitOutputWriterSmallOutputUnchanged pins the no-regression contract:
// output that fits the cap is byte-identical to what CombinedOutput returned,
// including the NUL/non-UTF-8 bytes a binary file's diff carries, and carries
// no truncation notice.
func TestGitOutputWriterSmallOutputUnchanged(t *testing.T) {
	payload := "a\x00b\xff\nsecond line\n"
	out := newGitOutput(1024)
	// Two writes, like stdout and stderr arriving on separate pipes.
	if _, err := out.Write([]byte(payload[:5])); err != nil {
		t.Fatal(err)
	}
	if _, err := out.Write([]byte(payload[5:])); err != nil {
		t.Fatal(err)
	}
	if got := out.String(); got != payload {
		t.Fatalf("small output = %q, want %q (verbatim, no notice)", got, payload)
	}
	if out.head.overflowed {
		t.Fatal("small output must not report overflow")
	}
}

// TestGitOutputWriterUncapped pins max_tool_result_bytes: 0 still means "no
// cap", so the whole stream is returned with no notice.
func TestGitOutputWriterUncapped(t *testing.T) {
	chunk := strings.Repeat("0123456789abcdef", 64) // 1 KiB
	out := newGitOutput(0)
	for i := 0; i < 64; i++ {
		if _, err := out.Write([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	got := out.String()
	if len(got) != 64*len(chunk) {
		t.Fatalf("uncapped output = %d bytes, want %d", len(got), 64*len(chunk))
	}
	if strings.Contains(got, "truncated") {
		t.Fatal("uncapped output must not carry a truncation notice")
	}
}

// TestGitOutputWriterBounded pins the memory bound: the head keeps exactly
// the first max bytes and the tail window never exceeds its own budget,
// however much git prints.
func TestGitOutputWriterBounded(t *testing.T) {
	const max = 16 * 1024
	chunk := []byte(strings.Repeat("0123456789abcdef", 64)) // 1 KiB
	out := newGitOutput(max)
	const writes = 100
	for i := 0; i < writes; i++ {
		if _, err := out.Write(chunk); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(out.head.String()); got != max {
		t.Fatalf("head buffer = %d bytes, want exactly the cap %d", got, max)
	}
	if !out.head.overflowed {
		t.Fatal("head must report overflow")
	}
	if out.tailMax != gitTailBytes {
		t.Fatalf("tailMax = %d, want %d (cap above the tail budget)", out.tailMax, gitTailBytes)
	}
	if got := len(out.tail); got != gitTailBytes {
		t.Fatalf("tail buffer = %d bytes, want %d", got, gitTailBytes)
	}
	if want := int64(writes * len(chunk)); out.total != want {
		t.Fatalf("total = %d, want %d", out.total, want)
	}
	text := out.String()
	notice := fmt.Sprintf("\n… truncated (output exceeds %d bytes)", max)
	// head + gap line + tail + notice, and nothing else.
	if want := max + len(gitOutputGap) + gitTailBytes + len(notice); len(text) > want {
		t.Fatalf("truncated text = %d bytes, want <= %d (head+gap+tail+notice)", len(text), want)
	}
	if !strings.HasPrefix(text, string(chunk)) {
		t.Fatal("truncated text must keep the FIRST bytes of the stream")
	}
	if !strings.HasSuffix(text, notice) {
		t.Fatalf("truncated text must END with the standard notice, got tail %q", text[len(text)-80:])
	}
	if !contextmgr.HasTruncatedTail(text) {
		t.Fatal("the notice must terminate the body (the shape every cap footer writes)")
	}
}

// TestGitOutputWriterKeepsFailureTail pins the reason the tail exists: a
// failing git command prints megabytes of diff and then its reason on the
// last line, which a head-only cap would drop.
func TestGitOutputWriterKeepsFailureTail(t *testing.T) {
	const max = 512
	out := newGitOutput(max)
	line := []byte(strings.Repeat("diff --git a/big b/big\n+line of a huge diff\n", 128))
	for i := 0; i < 64; i++ {
		if _, err := out.Write(line); err != nil {
			t.Fatal(err)
		}
	}
	fatal := "fatal: bad revision 'origin/main...HEAD'\n"
	if _, err := out.Write([]byte(fatal)); err != nil {
		t.Fatal(err)
	}
	// runGitCommand trims the result; match that here.
	text := strings.TrimSpace(out.String())
	if !strings.Contains(text, "fatal: bad revision") {
		t.Fatalf("truncated failure text lost git's trailing error: %q", text[len(text)-200:])
	}
	if !strings.Contains(text, "… truncated (output exceeds 512 bytes)") {
		t.Fatalf("truncated failure text lost the notice: %q", text[len(text)-200:])
	}
	if !out.head.overflowed {
		t.Fatal("expected the head to have overflowed")
	}
}

// TestGitOutputWriterTailClampedToCap pins that a small cap does not double:
// the tail window is clamped to the cap it accompanies.
func TestGitOutputWriterTailClampedToCap(t *testing.T) {
	out := newGitOutput(64)
	if out.tailMax != 64 {
		t.Fatalf("tailMax = %d, want the cap 64", out.tailMax)
	}
	if _, err := out.Write([]byte(strings.Repeat("x", 4096))); err != nil {
		t.Fatal(err)
	}
	text := out.String()
	notice := "\n… truncated (output exceeds 64 bytes)"
	if want := 64 + len(gitOutputGap) + 64 + len(notice); len(text) > want {
		t.Fatalf("truncated text = %d bytes, want <= %d", len(text), want)
	}
}

// TestGitOutputWriterOverlapNotDuplicated pins the overlap rule: when the
// stream is barely past the cap, the head already holds the beginning of the
// tail window, so those bytes are not printed twice — the capped result is
// the whole (short) stream with the gap line inserted at the cut.
func TestGitOutputWriterOverlapNotDuplicated(t *testing.T) {
	const max = 64
	out := newGitOutput(max)
	payload := strings.Repeat("0123456789", 10) // 100 bytes: 64 head + 36 kept tail
	if _, err := out.Write([]byte(payload)); err != nil {
		t.Fatal(err)
	}
	notice := fmt.Sprintf("\n… truncated (output exceeds %d bytes)", max)
	want := payload[:max] + gitOutputGap + payload[max:] + notice
	if got := out.String(); got != want {
		t.Fatalf("overlapping output = %q, want %q (no duplicated bytes)", got, want)
	}
}

// TestGitLogSmallOutputMatchesGitCommand pins the no-behaviour-change
// contract against real git: with a cap above the output size, GitLog returns
// exactly what the command prints.
func TestGitLogSmallOutputMatchesGitCommand(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	commitFile(t, dir, "a.txt", "one\n", "add a.txt")

	exec := NewExecutor(dir)
	exec.SetMaxToolOutputBytes(64 * 1024)
	out, err := exec.GitLog(context.Background(), "", 0)
	if err != nil {
		t.Fatal(err)
	}
	want := runGitCmd(t, dir, "log", "--oneline", "--no-decorate", "-n", "20")
	if out != want {
		t.Fatalf("GitLog = %q, want the raw git output %q", out, want)
	}
}

// TestGitLogOutputCapped pins the fix on a real repository: a `git log`
// bigger than the cap keeps a bounded head, the tail (git's oldest entries,
// where a failure message would sit) and the truncation notice.
func TestGitLogOutputCapped(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	var oldest string
	for i := 0; i < 20; i++ {
		subject := fmt.Sprintf("commit-%02d-%s", i, strings.Repeat("x", 180))
		commitFile(t, dir, "f.txt", fmt.Sprintf("v%d\n", i), subject)
		if i == 0 {
			oldest = subject
		}
	}

	const max = 256
	exec := NewExecutor(dir)
	exec.SetMaxToolOutputBytes(max)
	// 100 = gitLogMaxLimit: every commit, including the oldest one below.
	out, err := exec.GitLog(context.Background(), "", gitLogMaxLimit)
	if err != nil {
		t.Fatal(err)
	}
	notice := fmt.Sprintf("\n… truncated (output exceeds %d bytes)", max)
	if !strings.HasSuffix(out, notice) {
		t.Fatalf("capped log must end with the notice, got %q", out[len(out)-120:])
	}
	if want := max + len(gitOutputGap) + max + len(notice); len(out) > want {
		t.Fatalf("capped log = %d bytes, want <= %d (head+gap+tail+notice)", len(out), want)
	}
	// The head keeps the newest entry; the tail keeps the oldest one.
	if !strings.Contains(out[:max], "commit-19-") {
		t.Fatalf("capped log lost the newest commits: %q", out[:max])
	}
	if !strings.Contains(out, oldest) {
		t.Fatalf("capped log lost the oldest commit line %q: %q", oldest, out)
	}
}

// TestGitStagedDiffOutputCapped pins the fix on the other unbounded producer:
// a huge `git diff --cached` is capped with the head (hunk header), the tail
// (the last added lines) and the notice.
func TestGitStagedDiffOutputCapped(t *testing.T) {
	dir := t.TempDir()
	gitInit(t, dir)
	var sb strings.Builder
	for i := 0; i < 4000; i++ {
		fmt.Fprintf(&sb, "line-%05d-%s\n", i, strings.Repeat("y", 40))
	}
	if err := os.WriteFile(filepath.Join(dir, "big.txt"), []byte(sb.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	runGitCmd(t, dir, "add", "big.txt")

	const max = 2048
	exec := NewExecutor(dir)
	exec.SetMaxToolOutputBytes(max)
	out, err := exec.GitStagedDiff(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	notice := fmt.Sprintf("\n… truncated (output exceeds %d bytes)", max)
	if !strings.HasSuffix(out, notice) {
		t.Fatalf("capped diff must end with the notice, got %q", out[len(out)-120:])
	}
	if want := max + len(gitOutputGap) + max + len(notice); len(out) > want {
		t.Fatalf("capped diff = %d bytes, want <= %d (head+gap+tail+notice)", len(out), want)
	}
	if !strings.HasPrefix(out, "diff --git") {
		t.Fatalf("capped diff must keep its head, got %q", out[:40])
	}
	if !strings.Contains(out, "+line-03999-") {
		t.Fatalf("capped diff lost the last added lines: %q", out[len(out)-200:])
	}
}

// TestRunGitCommandFailureKeepsStderrText pins the merged-stream contract on
// a real failing command: git's stderr text comes back with the error.
func TestRunGitCommandFailureKeepsStderrText(t *testing.T) {
	dir := t.TempDir() // no git repo here
	text, err := NewExecutor(dir).runGitCommand(context.Background(), []string{"log", "--oneline"})
	if err == nil {
		t.Fatalf("expected an error outside a repository, got %q", text)
	}
	if !strings.Contains(text, "fatal:") {
		t.Fatalf("failing git text = %q, want git's stderr text", text)
	}
	if !strings.Contains(err.Error(), "git log failed:") {
		t.Fatalf("error = %v, want the wrapped subcommand name", err)
	}
}
