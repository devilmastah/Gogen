package agent

import (
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// PinnedMessage is a message that survives context compaction.
type PinnedMessage struct {
	Index int // index into canonical messages at pin time
	Msg   llm.Message
}

// PinManager tracks pinned message indices that must survive compaction.
type PinManager struct {
	pinned map[int]struct{} // indices into canonical history
}

// NewPinManager creates a pin manager.
func NewPinManager() *PinManager {
	return &PinManager{pinned: make(map[int]struct{})}
}

// PinLastUser pins the most recent real user message so it survives
// compaction (see contextmgr.IsRequest). Compaction summaries are skipped:
// legacy sessions stored them as user-role messages, and pinning one would
// preserve a recap instead of the actual last user message. Machine
// messages are skipped too: pinning a job notice or a read_image caption
// would keep that (and its image) instead of the request.
func (p *PinManager) PinLastUser(messages []llm.Message) {
	for i := len(messages) - 1; i >= 0; i-- {
		if contextmgr.IsRequest(messages[i]) {
			p.pinned[i] = struct{}{}
			return
		}
	}
}

// ClearPins removes all pins.
func (p *PinManager) ClearPins() {
	p.pinned = make(map[int]struct{})
}

// PinnedSet returns a copy of the pinned index set.
func (p *PinManager) PinnedSet() map[int]struct{} {
	if p == nil || len(p.pinned) == 0 {
		return nil
	}
	out := make(map[int]struct{}, len(p.pinned))
	for idx := range p.pinned {
		out[idx] = struct{}{}
	}
	return out
}

// ReplacePins replaces the pin set (used after compaction remaps indices).
func (p *PinManager) ReplacePins(pins map[int]struct{}) {
	if p == nil {
		return
	}
	if len(pins) == 0 {
		p.pinned = make(map[int]struct{})
		return
	}
	p.pinned = make(map[int]struct{}, len(pins))
	for idx := range pins {
		p.pinned[idx] = struct{}{}
	}
}

// pinLastUser pins the most recent user message so it survives compaction.
// When no PinManager is configured the tool degrades to a no-op (this only
// happens in tests/custom embeds) so the LLM sees a successful acknowledgement
// rather than a confusing error.
func (a *Agent) pinLastUser() error {
	if a.PinManager == nil {
		return nil
	}
	a.PinManager.PinLastUser(a.Messages)
	return nil
}
func pinnedSet(p *PinManager) map[int]struct{} {
	if p == nil {
		return nil
	}
	return p.PinnedSet()
}
