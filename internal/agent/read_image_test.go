package agent

import (
	"context"
	"encoding/base64"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// png1x1Base64 is a 1x1 transparent PNG, used as a realistic image fixture.
const png1x1Base64 = "iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// mustDecodePNG decodes the 1x1 PNG fixture.
func mustDecodePNG(t *testing.T) []byte {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(png1x1Base64)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func writeTestImage(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

// TestReadImageAttachesImageAfterToolResult covers the sequential happy path:
// the transcript must read assistant(tool_call) → tool(result) → user(image
// + caption), with the image transported as a base64 data URL.
func TestReadImageAttachesImageAfterToolResult(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	writeTestImage(t, filepath.Join(dir, "pic.png"), mustDecodePNG(t))

	tc := llm.ToolCall{ID: "tc1", Name: "read_image", Args: map[string]any{"path": "pic.png"}}
	a.appendMessage(llm.Message{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{tc}})

	if outcome, _ := a.runToolRound(context.Background(), &llm.StreamHandlers{}, []llm.ToolCall{tc}); outcome != toolRoundContinue {
		t.Fatal("tool round reported cancellation")
	}

	msgs := a.Messages
	if len(msgs) != 3 {
		t.Fatalf("messages = %d, want 3 (assistant, tool, user): %+v", len(msgs), msgs)
	}
	if msgs[1].Role != "tool" || msgs[1].ToolCallID != "tc1" {
		t.Fatalf("message 1 = %+v, want tool result for tc1", msgs[1])
	}
	if !strings.Contains(msgs[1].Content, "Image attached to context: pic.png") {
		t.Fatalf("tool ack = %q, want attachment notice", msgs[1].Content)
	}
	if msgs[2].Role != "user" || len(msgs[2].Images) != 1 {
		t.Fatalf("message 2 = %+v, want user message with exactly one image", msgs[2])
	}
	img := msgs[2].Images[0]
	if !strings.HasPrefix(img.DataURL, "data:image/png;base64,") {
		t.Fatalf("data URL = %q, want image/png base64", img.DataURL)
	}
	if img.Detail != "" {
		t.Fatalf("detail = %q, want empty (defaults to auto)", img.Detail)
	}
	if !strings.Contains(msgs[2].Content, "[read_image] pic.png") {
		t.Fatalf("caption = %q, want [read_image] prefix with relpath", msgs[2].Content)
	}
	if contextmgr.IsRequest(msgs[2]) {
		t.Fatalf("the image message %q counts as a user request", msgs[2].Content)
	}
}

// TestReadImageParallelAppendsImagesInModelOrder covers the parallel batch
// path: two read_image calls in one round must yield
// tool(tc1) → tool(tc2) → user(a.png) → user(b.png) — the whole tool batch
// closes before the images, because strict providers require the tool
// messages answering one assistant tool_calls message to directly follow it
// — with images paired to their tool results in the model's call order.
func TestReadImageParallelAppendsImagesInModelOrder(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	writeTestImage(t, filepath.Join(dir, "a.png"), mustDecodePNG(t))
	writeTestImage(t, filepath.Join(dir, "b.png"), mustDecodePNG(t))

	calls := []llm.ToolCall{
		{ID: "tc1", Name: "read_image", Args: map[string]any{"path": "a.png"}},
		{ID: "tc2", Name: "read_image", Args: map[string]any{"path": "b.png"}},
	}
	a.appendMessage(llm.Message{Role: "assistant", Content: "", ToolCalls: calls})

	if outcome, _ := a.runToolRound(context.Background(), &llm.StreamHandlers{}, calls); outcome != toolRoundContinue {
		t.Fatal("tool round reported cancellation")
	}

	msgs := a.Messages
	if len(msgs) != 5 {
		t.Fatalf("messages = %d, want 5 (assistant, tool, tool, user, user): %+v", len(msgs), msgs)
	}
	wantRoles := []string{"tool", "tool", "user", "user"}
	for i, role := range wantRoles {
		if msgs[i+1].Role != role {
			t.Fatalf("message %d role = %q, want %q (order is protocol-sensitive)", i+1, msgs[i+1].Role, role)
		}
	}
	if msgs[1].ToolCallID != "tc1" || msgs[2].ToolCallID != "tc2" {
		t.Fatalf("tool results out of model order: %q, %q", msgs[1].ToolCallID, msgs[2].ToolCallID)
	}
	if !strings.Contains(msgs[3].Content, "a.png") || !strings.Contains(msgs[4].Content, "b.png") {
		t.Fatalf("image captions out of model order: %q, %q", msgs[3].Content, msgs[4].Content)
	}
}

// TestReadImageNotLastInSequentialBatchKeepsToolMessagesContiguous pins the
// wire-order rule for the sequential path: a read_image call that is NOT the
// last call of its batch must not push its image user message between two
// tool messages. The transcript must read tool(tc1) → tool(tc2) →
// user(image) — otherwise strict OpenAI-compatible providers reject the next
// request with "insufficient tool messages following tool_calls message".
func TestReadImageNotLastInSequentialBatchKeepsToolMessagesContiguous(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	writeTestImage(t, filepath.Join(dir, "a.png"), mustDecodePNG(t))

	// execute_command is not read-only, so the batch cannot take the
	// parallel path — this exercises runToolRound's sequential loop.
	calls := []llm.ToolCall{
		{ID: "tc1", Name: "read_image", Args: map[string]any{"path": "a.png"}},
		{ID: "tc2", Name: "execute_command", Args: map[string]any{"command": "echo hi"}},
	}
	a.appendMessage(llm.Message{Role: "assistant", Content: "", ToolCalls: calls})

	if outcome, _ := a.runToolRound(context.Background(), &llm.StreamHandlers{}, calls); outcome != toolRoundContinue {
		t.Fatal("tool round did not continue")
	}

	msgs := a.Messages
	if len(msgs) != 4 {
		t.Fatalf("messages = %d, want 4 (assistant, tool, tool, user): %+v", len(msgs), msgs)
	}
	wantRoles := []string{"tool", "tool", "user"}
	for i, role := range wantRoles {
		if msgs[i+1].Role != role {
			t.Fatalf("message %d role = %q, want %q (tool messages must stay contiguous)", i+1, msgs[i+1].Role, role)
		}
	}
	if msgs[1].ToolCallID != "tc1" || msgs[2].ToolCallID != "tc2" {
		t.Fatalf("tool results out of model order: %q, %q", msgs[1].ToolCallID, msgs[2].ToolCallID)
	}
	if len(msgs[3].Images) != 1 || !strings.Contains(msgs[3].Content, "a.png") {
		t.Fatalf("message 3 = %+v, want the image user message after the batch", msgs[3])
	}
}

// TestReadImageRejectsPathEscape verifies the SecurePath workspace-boundary
// protection applies to read_image exactly as it does to read_file.
func TestReadImageRejectsPathEscape(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)

	_, err := a.executeTool(context.Background(), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "../outside.png"}})
	if err == nil || !strings.Contains(err.Error(), "outside of allowed boundary") {
		t.Fatalf("expected boundary error, got %v", err)
	}
}

// TestReadImageRejectsOversized verifies the raw-file size cap (keeps the
// base64 data URL under the server's 5 MB attach cap).
func TestReadImageRejectsOversized(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	writeTestImage(t, filepath.Join(dir, "huge.png"), make([]byte, readImageMaxBytes+1))

	_, err := a.executeTool(context.Background(), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "huge.png"}})
	if err == nil || !strings.Contains(err.Error(), "supports files up to") {
		t.Fatalf("expected size-cap error, got %v", err)
	}
}

// TestReadImageRejectsNonImage verifies plain text is refused.
func TestReadImageRejectsNonImage(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	writeTestImage(t, filepath.Join(dir, "notes.txt"), []byte("hello world, not an image"))

	_, err := a.executeTool(context.Background(), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "notes.txt"}})
	if err == nil || !strings.Contains(err.Error(), "not a supported image") {
		t.Fatalf("expected non-image error, got %v", err)
	}
}

// TestReadImageRejectsSVG verifies SVG is refused with a read_file hint: it
// is XML text the model can read directly.
func TestReadImageRejectsSVG(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	writeTestImage(t, filepath.Join(dir, "vector.svg"), []byte("<svg xmlns='http://www.w3.org/2000/svg'><rect width='1' height='1'/></svg>"))

	_, err := a.executeTool(context.Background(), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "vector.svg"}})
	if err == nil || !strings.Contains(err.Error(), "use read_file") {
		t.Fatalf("expected SVG rejection with read_file hint, got %v", err)
	}
}

// TestReadImageRejectsDirectory verifies directories are refused.
func TestReadImageRejectsDirectory(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	if err := os.Mkdir(filepath.Join(dir, "imgs"), 0o755); err != nil {
		t.Fatal(err)
	}

	_, err := a.executeTool(context.Background(), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "imgs"}})
	if err == nil || !strings.Contains(err.Error(), "directory") {
		t.Fatalf("expected directory error, got %v", err)
	}
}

// TestReadImageDetailForwarding verifies the detail level is validated and
// forwarded into the ImageInput.
func TestReadImageDetailForwarding(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	writeTestImage(t, filepath.Join(dir, "pic.png"), mustDecodePNG(t))

	tc := llm.ToolCall{ID: "tc1", Name: "read_image", Args: map[string]any{"path": "pic.png", "detail": "high"}}
	a.appendMessage(llm.Message{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{tc}})
	if outcome, _ := a.runToolRound(context.Background(), &llm.StreamHandlers{}, []llm.ToolCall{tc}); outcome != toolRoundContinue {
		t.Fatal("tool round reported cancellation")
	}
	if got := a.Messages[2].Images[0].Detail; got != "high" {
		t.Fatalf("detail = %q, want high", got)
	}

	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "pic.png", "detail": "bogus"}}); err == nil || !strings.Contains(err.Error(), "invalid detail") {
		t.Fatalf("expected invalid-detail error, got %v", err)
	}
}

// TestReadImageExtensionFallback verifies a file whose content is not
// sniffable (DetectContentType returns application/octet-stream) is accepted
// when its extension names a known image type.
func TestReadImageExtensionFallback(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	// A NUL byte forces DetectContentType to application/octet-stream.
	writeTestImage(t, filepath.Join(dir, "blob.png"), []byte{0x00, 0x01, 0x02, 0x03})

	tc := llm.ToolCall{ID: "tc1", Name: "read_image", Args: map[string]any{"path": "blob.png"}}
	a.appendMessage(llm.Message{Role: "assistant", Content: "", ToolCalls: []llm.ToolCall{tc}})
	if outcome, _ := a.runToolRound(context.Background(), &llm.StreamHandlers{}, []llm.ToolCall{tc}); outcome != toolRoundContinue {
		t.Fatal("tool round reported cancellation")
	}
	if got := a.Messages[2].Images[0].DataURL; !strings.HasPrefix(got, "data:image/png;base64,") {
		t.Fatalf("data URL = %q, want image/png via extension fallback", got)
	}
}

// TestReadImageSessionCap verifies the per-session soft cap: the
// (maxSessionImages+1)-th attachment is refused. Handlers are exercised
// directly with a context carrying an image sink, so the cap check itself is
// what is under test.
func TestReadImageSessionCap(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	writeTestImage(t, filepath.Join(dir, "pic.png"), mustDecodePNG(t))

	for i := 0; i < maxSessionImages; i++ {
		if _, err := a.executeTool(ContextWithImageSink(context.Background(), &imageSink{}), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "pic.png"}}); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	_, err := a.executeTool(ContextWithImageSink(context.Background(), &imageSink{}), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "pic.png"}})
	if err == nil || !strings.Contains(err.Error(), "session image limit reached") {
		t.Fatalf("expected session-limit error on the %d-th attachment, got %v", maxSessionImages+1, err)
	}
}

// TestReadImageRequiresImageSink pins the invariant that a read_image call
// executed outside the tool-round machinery (no image sink in context) fails
// instead of silently dropping the image.
func TestReadImageRequiresImageSink(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	writeTestImage(t, filepath.Join(dir, "pic.png"), mustDecodePNG(t))

	_, err := a.executeTool(context.Background(), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "pic.png"}})
	if err == nil || !strings.Contains(err.Error(), "no image sink") {
		t.Fatalf("expected missing-sink error, got %v", err)
	}
}

// TestReadImageSessionCapResetsOnNewSession pins the TUI /new path: the
// per-session cap must reset when the session state is reset, not persist
// for the agent's lifetime.
func TestReadImageSessionCapResetsOnNewSession(t *testing.T) {
	dir := t.TempDir()
	exec := NewExecutor(dir)
	a := NewAgent(nil, exec, nil)
	writeTestImage(t, filepath.Join(dir, "pic.png"), mustDecodePNG(t))

	for i := 0; i < maxSessionImages; i++ {
		if _, err := a.executeTool(ContextWithImageSink(context.Background(), &imageSink{}), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "pic.png"}}); err != nil {
			t.Fatalf("call %d: %v", i+1, err)
		}
	}
	a.ResetSessionState()
	if _, err := a.executeTool(ContextWithImageSink(context.Background(), &imageSink{}), llm.ToolCall{Name: "read_image", Args: map[string]any{"path": "pic.png"}}); err != nil {
		t.Fatalf("cap must reset with the session: %v", err)
	}
}

// TestReadImageCapRecountsOnReplace pins the compaction/restore direction:
// the budget tracks images present in the message list, so replacing the
// conversation re-derives it — images that survive keep counting, dropped
// ones release their slots.
func TestReadImageCapRecountsOnReplace(t *testing.T) {
	exec := NewExecutor(t.TempDir())
	a := NewAgent(nil, exec, nil)

	// Exhaust the budget through direct reservations (the same claim the
	// handler performs) without appending messages.
	for i := 0; i < maxSessionImages; i++ {
		if !a.reserveImageSlot() {
			t.Fatalf("reservation %d failed", i+1)
		}
	}
	if a.sessionImages.Load() != maxSessionImages {
		t.Fatalf("counter = %d, want %d", a.sessionImages.Load(), maxSessionImages)
	}
	// Compaction/restore replace the conversation with a list that holds
	// exactly one image: the budget must re-derive from the messages.
	one := llm.Message{Role: "user", Content: "keep", Images: []llm.ImageInput{{DataURL: "data:image/png;base64,"}}}
	a.replaceMessages([]llm.Message{one})
	if got := a.sessionImages.Load(); got != 1 {
		t.Fatalf("counter after replace = %d, want 1", got)
	}
	// A fresh session resets the budget to zero (the TUI /new path).
	a.ResetSessionState()
	if got := a.sessionImages.Load(); got != 0 {
		t.Fatalf("counter after reset = %d, want 0", got)
	}
}
