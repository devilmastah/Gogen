package tui

import (
	"context"
	"fmt"
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"

	"gogen/internal/agent"
	"gogen/internal/llm"
)

// groupHeaderText composes exactly what buildModelRows emits for a provider
// group header (same vars, so it holds under NO_COLOR too).
func groupHeaderText(name string) string {
	return "  " + ansiCyanOn + name + ansiReset
}

func TestBuildModelRows(t *testing.T) {
	tests := []struct {
		name        string
		list        []llm.ModelInfo
		start       int
		end         int
		cursor      int
		wantHeaders []string // in order, one per emitted group header
		wantNumbers []int    // 1-based numbers that must appear as rows
	}{
		{
			name: "groups consecutive providers under styled headers",
			list: []llm.ModelInfo{
				{ID: "gpt-4o", Provider: "default"},
				{ID: "gpt-4o-mini", Provider: "default"},
				{ID: "llama3", Provider: "ollama"},
			},
			start: 0, end: 3, cursor: 0,
			wantHeaders: []string{"default", "ollama"},
			wantNumbers: []int{1, 2, 3},
		},
		{
			name: "empty provider tag falls back to default",
			list: []llm.ModelInfo{
				{ID: "m1"},
				{ID: "m2", Provider: ""},
				{ID: "m3", Provider: "default"},
			},
			start: 0, end: 3, cursor: 0,
			wantHeaders: []string{"default"},
			wantNumbers: []int{1, 2, 3},
		},
		{
			name: "resuming mid-group emits no duplicate header",
			list: []llm.ModelInfo{
				{ID: "a1", Provider: "p"}, {ID: "a2", Provider: "p"}, {ID: "a3", Provider: "p"},
				{ID: "a4", Provider: "p"}, {ID: "a5", Provider: "p"},
			},
			start: 2, end: 5, cursor: 3,
			wantHeaders: nil,
			wantNumbers: []int{3, 4, 5},
		},
		{
			name: "headers appear for groups starting within the window",
			list: []llm.ModelInfo{
				{ID: "x1", Provider: "aa"}, {ID: "x2", Provider: "aa"},
				{ID: "y1", Provider: "bb"}, {ID: "y2", Provider: "bb"},
				{ID: "z1", Provider: "cc"},
			},
			start: 2, end: 5, cursor: 2,
			wantHeaders: []string{"bb", "cc"},
			wantNumbers: []int{3, 4, 5},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rows, _ := buildModelRows(tt.list, tt.start, tt.end, tt.cursor)

			var gotHeaders []string
			for _, r := range rows {
				if !r.preStyled {
					continue
				}
				for _, prov := range tt.wantHeaders {
					if r.text == groupHeaderText(prov) {
						gotHeaders = append(gotHeaders, prov)
						break
					}
				}
			}
			if fmt.Sprint(gotHeaders) != fmt.Sprint(tt.wantHeaders) {
				t.Fatalf("group headers = %v, want %v", gotHeaders, tt.wantHeaders)
			}

			for _, n := range tt.wantNumbers {
				want := fmt.Sprintf("  %2d. ", n)
				found := false
				for _, r := range rows {
					if !r.preStyled && strings.HasPrefix(r.text, want) {
						found = true
						break
					}
				}
				if !found {
					t.Errorf("model number %d missing from rendered rows", n)
				}
			}
		})
	}
}

func TestBuildModelRowsHighlightsCursor(t *testing.T) {
	list := []llm.ModelInfo{
		{ID: "a", Provider: "p"},
		{ID: "b", Provider: "q"},
		{ID: "c", Provider: "q", Current: true},
	}
	rows, _ := buildModelRows(list, 0, 3, 2)

	highlighted := 0
	for _, r := range rows {
		if r.preStyled {
			continue
		}
		if r.highlight {
			highlighted++
			if !strings.HasSuffix(r.text, "c") && !strings.Contains(r.text, " c") {
				t.Fatalf("highlighted row is not the cursor model: %q", r.text)
			}
		}
	}
	if highlighted != 1 {
		t.Fatalf("got %d highlighted rows, want exactly 1", highlighted)
	}
}

func TestBuildModelRowsContextAndCurrentSuffix(t *testing.T) {
	list := []llm.ModelInfo{{ID: "glm-5", Provider: "z", ContextLimit: 128000, Current: true}}
	rows, _ := buildModelRows(list, 0, 1, 0)

	var entry *styleLine
	for i := range rows {
		if !rows[i].preStyled {
			entry = &rows[i]
		}
	}
	if entry == nil {
		t.Fatal("no model entry row rendered")
	}
	want := "   1. glm-5  (context: 128000 tokens) *"
	if entry.text != want {
		t.Fatalf("entry row = %q, want %q", entry.text, want)
	}
}

func TestRenderModelsModalShowsGroups(t *testing.T) {
	m := &Model{
		modelList: []llm.ModelInfo{
			{ID: "gpt-4o", Provider: "default", ContextLimit: 128000, Current: true},
			{ID: "llama3", Provider: "ollama", ContextLimit: 32768},
		},
		modelCursor: 0,
		height:      40,
	}
	out := m.renderModelsModal()
	if !strings.Contains(out, groupHeaderText("default")) ||
		!strings.Contains(out, groupHeaderText("ollama")) {
		t.Fatalf("modal output missing provider group headers:\n%s", out)
	}
	// Numbering stays aligned with /models <n> across group boundaries:
	// the second model keeps number 2 even though it sits under a new header.
	if !strings.Contains(out, "   2. llama3") {
		t.Fatalf("second model lost its continuous numbering:\n%s", out)
	}
}

func TestRenderModelsModalEmpty(t *testing.T) {
	m := &Model{}
	out := m.renderModelsModal()
	if !strings.Contains(out, "No models available.") {
		t.Fatalf("empty modal output unexpected:\n%s", out)
	}
}

// The picker's width has three regimes: the fixed preferred default for
// short catalogs (no hugging), growth to fit the biggest model name when
// the terminal is wide enough, and truncation under the terminal clamp
// when it is not. The probe measures the WHOLE list, so scrolling between
// windows that include/exclude the longest entry must never resize the box.
func TestRenderModelsModalStableWidth(t *testing.T) {
	boxWidth := func(t *testing.T, list []llm.ModelInfo, termW int) int {
		t.Helper()
		m := &Model{modelList: list, width: termW, height: 40}
		out := m.renderModelsModal()
		lines := strings.Split(out, "\n")
		top := lipgloss.Width(lines[0])
		for i, l := range lines {
			if w := lipgloss.Width(l); w != top {
				t.Fatalf("ragged modal box at row %d: border %d vs row %d", i, top, w)
			}
		}
		return top
	}

	short := []llm.ModelInfo{{ID: "m1"}, {ID: "m2"}}
	longID := "anthropic/claude-sonnet-4-5-20250929-with-a-very-long-provider-qualified-identifier"
	long := []llm.ModelInfo{{ID: longID, Provider: "default", ContextLimit: 200000}}

	t.Run("short ids get the preferred width", func(t *testing.T) {
		if got := boxWidth(t, short, 100); got != modelsModalInnerWidth+2 {
			t.Fatalf("box = %d cells, want the preferred %d", got, modelsModalInnerWidth+2)
		}
	})

	t.Run("biggest name fits when the terminal is wide enough", func(t *testing.T) {
		const termW = 200
		want := lipgloss.Width(modelEntryText(0, long[0])) + 6 // padding + borders
		if got := boxWidth(t, long, termW); got != want {
			t.Fatalf("box = %d cells, want %d (the longest entry fully visible)", got, want)
		}
		m := &Model{modelList: long, width: termW, height: 40}
		if plain := stripANSI(m.renderModelsModal()); !strings.Contains(plain, longID+"  (context: 200000 tokens)") {
			t.Fatal("wide terminal truncated an id that fits")
		}
	})

	t.Run("names wider than the screen grow to the clamp and truncate there", func(t *testing.T) {
		// limit = 100-6 inner -> total 96; the natural width exceeds it.
		if got := boxWidth(t, long, 100); got != 96 {
			t.Fatalf("box = %d cells, want the clamped %d", got, 100-4)
		}
		m := &Model{modelList: long, width: 100, height: 40}
		plain := stripANSI(m.renderModelsModal())
		// The 85-char id itself still fits inside the clamped row; what
		// must be cut is the trailing context suffix beyond the budget.
		if !strings.Contains(plain, longID) {
			t.Fatal("clamped row lost an id that still fits within the clamp")
		}
		if strings.Contains(plain, "(context: 200000 tokens)") {
			t.Fatal("row content beyond the terminal clamp was not truncated")
		}
	})

	t.Run("width does not change while scrolling past the long entry", func(t *testing.T) {
		list := make([]llm.ModelInfo, 0, 30)
		for i := 0; i < 29; i++ {
			list = append(list, llm.ModelInfo{ID: fmt.Sprintf("short-model-%d", i)})
		}
		list = append(list, llm.ModelInfo{ID: longID, ContextLimit: 200000})
		atTop := boxWidth(t, list, 120) // window [0..27): excludes index 29
		m := &Model{modelList: list, modelCursor: 29, width: 120, height: 40}
		bottom := lipgloss.Width(strings.Split(m.renderModelsModal(), "\n")[0]) // includes index 29
		if atTop != bottom {
			t.Fatalf("box resized while scrolling: %d -> %d cells", atTop, bottom)
		}
	})

	t.Run("empty catalog matches the populated picker width", func(t *testing.T) {
		m := &Model{width: 100, height: 40}
		if got := lipgloss.Width(m.renderModelsModal()); got != modelsModalInnerWidth+2 {
			t.Fatalf("empty-catalog box = %d cells, want %d", got, modelsModalInnerWidth+2)
		}
	})

	t.Run("narrow terminal still clamps", func(t *testing.T) {
		// limit = 50-6 inner -> total 46; preferred 58 must give way.
		if got := boxWidth(t, short, 50); got > 46 {
			t.Fatalf("box = %d cells in a 50-col terminal, want <= 46", got)
		}
	})
}

// modelsTestProvider is a minimal LLMProvider with a per-model
// reasoning-effort table for the models-modal thinking-chip tests: the
// agent's ThinkingLevelsForModel resolves through ModelReasoningEfforts.
// A model absent from the table gets the default set (unknown model); an
// empty slice is a KNOWN toggle-only model (no effort control).
type modelsTestProvider struct {
	current string
	catalog []llm.ModelInfo
	efforts map[string][]string
}

func (p *modelsTestProvider) GenerateResponse(ctx context.Context, messages []llm.Message, allowedTools map[string]struct{}, extraTools []llm.Tool) (llm.Response, error) {
	return llm.Response{}, nil
}

func (p *modelsTestProvider) GenerateResponseStream(ctx context.Context, messages []llm.Message, allowedTools map[string]struct{}, extraTools []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	return &llm.StreamResult{}, nil
}

func (p *modelsTestProvider) ModelContextLimit(ctx context.Context) (int, error) {
	return 100000, nil
}

func (p *modelsTestProvider) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	return p.catalog, nil
}

func (p *modelsTestProvider) SetModel(id string) error {
	p.current = id
	return nil
}

func (p *modelsTestProvider) ModelName() string { return p.current }

func (p *modelsTestProvider) SetThinkingLevel(level string) {}

func (p *modelsTestProvider) ModelReasoningEfforts(ref llm.ModelRef) []string {
	if e, ok := p.efforts[ref.ID]; ok {
		return e
	}
	return llm.DefaultReasoningEfforts
}

func TestModelsModalThinkingChips(t *testing.T) {
	tests := []struct {
		name       string
		list       []llm.ModelInfo
		cursor     int
		current    string
		efforts    map[string][]string
		wantChips  []string // chip labels that must appear
		wantAbsent string   // text that must NOT appear ("" = skip)
	}{
		{
			name:      "cursor model's accepted efforts",
			list:      []llm.ModelInfo{{ID: "m1"}, {ID: "m2"}},
			cursor:    0,
			current:   "m1",
			efforts:   map[string][]string{"m1": {"low", "medium", "high"}, "m2": {"high", "max"}},
			wantChips: []string{"[Off]", "[L]", "[M]", "[H]"},
		},
		{
			name:       "non-default efforts use full labels",
			list:       []llm.ModelInfo{{ID: "m1"}, {ID: "m2"}},
			cursor:     1,
			current:    "m1",
			efforts:    map[string][]string{"m1": {"low"}, "m2": {"high", "max"}},
			wantChips:  []string{"[Off]", "[H]", "[Max]"},
			wantAbsent: "[M]",
		},
		{
			name:       "toggle-only model hides the section",
			list:       []llm.ModelInfo{{ID: "m1"}},
			cursor:     0,
			current:    "m1",
			efforts:    map[string][]string{"m1": {}},
			wantAbsent: "Thinking level",
		},
		{
			name:      "empty catalog falls back to the current model",
			list:      nil,
			cursor:    0,
			current:   "m1",
			efforts:   map[string][]string{"m1": {"high"}},
			wantChips: []string{"[Off]", "[H]"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := &modelsTestProvider{current: tt.current, efforts: tt.efforts}
			m := &Model{
				agent:       &agent.Agent{Provider: p},
				modelList:   tt.list,
				modelCursor: tt.cursor,
				thinkingSel: agent.ThinkingOff,
				width:       100,
				height:      40,
			}
			out := stripANSI(m.renderModelsModal())
			for _, chip := range tt.wantChips {
				if !strings.Contains(out, chip) {
					t.Errorf("chip row missing %s:\n%s", chip, out)
				}
			}
			if tt.wantAbsent != "" && strings.Contains(out, tt.wantAbsent) {
				t.Errorf("output unexpectedly contains %q:\n%s", tt.wantAbsent, out)
			}
		})
	}
}

func TestModelsModalThinkingChipHighlight(t *testing.T) {
	if noColor {
		// NO_COLOR empties every style prefix, so "highlighted" cannot be told
		// apart from the chip's own text: the negative check below would then
		// read "the frame contains [Off]" (it always does) and fail. Asserting
		// the unstyled render here would only restate that NO_COLOR works.
		t.Skip("NO_COLOR strips the chip highlighting this test asserts")
	}
	p := &modelsTestProvider{current: "m1", efforts: map[string][]string{"m1": {"low", "medium", "high"}}}
	m := &Model{
		agent:       &agent.Agent{Provider: p},
		modelList:   []llm.ModelInfo{{ID: "m1"}},
		thinkingSel: agent.ThinkingHigh,
		width:       100,
		height:      40,
	}
	out := m.renderModelsModal()
	if !strings.Contains(out, ansiHighlightOn+"[H]"+ansiReset) {
		t.Fatalf("staged chip not highlighted:\n%s", stripANSI(out))
	}
	if strings.Contains(out, ansiHighlightOn+"[Off]"+ansiReset) {
		t.Fatal("Off chip highlighted although a level is staged")
	}
}

func TestModelsModalThinkingKeys(t *testing.T) {
	p := &modelsTestProvider{
		current: "m1",
		catalog: []llm.ModelInfo{{ID: "m1", Current: true}, {ID: "m2"}},
		efforts: map[string][]string{"m1": {"low", "medium", "high"}, "m2": {"high", "max"}},
	}
	m := &Model{
		agent:       &agent.Agent{Provider: p},
		modelList:   p.catalog,
		thinkingSel: agent.ThinkingOff,
		modal:       ModalModels,
		width:       100,
		height:      40,
	}

	// right/left move the staged selection, clamped at the ends.
	m.handleModelsKey(keyMsg("right"))
	if m.thinkingSel != agent.ThinkingLow {
		t.Fatalf("right staged %q, want low", m.thinkingSel)
	}
	m.handleModelsKey(keyMsg("right"))
	if m.thinkingSel != agent.ThinkingMedium {
		t.Fatalf("right staged %q, want medium", m.thinkingSel)
	}
	m.handleModelsKey(keyMsg("left"))
	if m.thinkingSel != agent.ThinkingLow {
		t.Fatalf("left staged %q, want low", m.thinkingSel)
	}

	// A staged level the next model accepts survives the cursor move…
	m.thinkingSel = agent.ThinkingHigh
	m.handleModelsKey(keyMsg("down")) // cursor -> m2 (accepts high)
	if m.thinkingSel != agent.ThinkingHigh {
		t.Fatalf("staged high lost moving to an accepting model: %q", m.thinkingSel)
	}
	// …one it does not accept resets to off (resetStaleThinking parity).
	m.thinkingSel = agent.ThinkingLevel("max")
	m.handleModelsKey(keyMsg("up")) // cursor -> m1 (no max)
	if m.thinkingSel != agent.ThinkingOff {
		t.Fatalf("staged max not reset for m1: %q", m.thinkingSel)
	}

	// esc discards the staging: the agent's level is untouched.
	m.thinkingSel = agent.ThinkingHigh
	m.handleModelsKey(keyMsg("esc"))
	if m.modal != ModalNone || m.agent.ThinkingLevel != "" {
		t.Fatalf("esc must discard staging: modal=%v level=%q", m.modal, m.agent.ThinkingLevel)
	}

	// enter on the CURRENT model applies only the staged level.
	m.modal = ModalModels
	m.modelCursor = 0 // m1, Current
	m.thinkingSel = agent.ThinkingHigh
	m.handleModelsKey(keyMsg("enter"))
	if m.modal != ModalNone || m.agent.ThinkingLevel != agent.ThinkingHigh {
		t.Fatalf("enter on current model must apply the staged level: modal=%v level=%q", m.modal, m.agent.ThinkingLevel)
	}
	if p.current != "m1" {
		t.Fatalf("provider model changed to %q, want m1", p.current)
	}

	// enter on a DIFFERENT model starts the async switch carrying the
	// staged level (applied post-switch by handleModelSwitchMsg).
	m.modal = ModalModels
	m.modelCursor = 1 // m2
	m.thinkingSel = agent.ThinkingLevel("max")
	_, cmd := m.handleModelsKey(keyMsg("enter"))
	if m.modal != ModalNone || cmd == nil {
		t.Fatal("enter on a different model must start the async switch")
	}
	msg, ok := cmd().(modelSwitchMsg)
	if !ok {
		t.Fatalf("switch cmd produced %T, want modelSwitchMsg", cmd())
	}
	if msg.agent != m.agent || msg.thinking != agent.ThinkingLevel("max") {
		t.Fatalf("staged level not carried: agent=%v thinking=%q", msg.agent == m.agent, msg.thinking)
	}
	if p.current != "m2" {
		t.Fatalf("provider model = %q, want m2", p.current)
	}
}

func TestModelsModalSwitchAppliesStagedThinking(t *testing.T) {
	p := &modelsTestProvider{
		current: "m2",
		efforts: map[string][]string{"m1": {"low", "medium", "high"}, "m2": {"high", "max"}},
	}
	m := &Model{
		agent:  &agent.Agent{Provider: p},
		width:  100,
		height: 40,
	}

	// Accepted level: applied after the switch, with its notice line.
	m.handleModelSwitchMsg(modelSwitchMsg{agent: m.agent, out: "Switched to model: m2", thinking: agent.ThinkingLevel("max")})
	if m.agent.ThinkingLevel != agent.ThinkingLevel("max") {
		t.Fatalf("staged level not applied: %q", m.agent.ThinkingLevel)
	}
	joined := stripANSI(strings.Join(m.chatLines, "\n"))
	if !strings.Contains(joined, "Thinking level set to Max.") {
		t.Fatalf("missing thinking notice line:\n%s", joined)
	}

	// Rejected level (the switch landed on m1, which has no max): not
	// stored, and the rejection is reported.
	p.current = "m1"
	m.agent.ThinkingLevel = agent.ThinkingOff
	m.handleModelSwitchMsg(modelSwitchMsg{agent: m.agent, out: "Switched to model: m1", thinking: agent.ThinkingLevel("max")})
	if m.agent.ThinkingLevel != agent.ThinkingOff {
		t.Fatalf("rejected level was stored: %q", m.agent.ThinkingLevel)
	}
	joined = stripANSI(strings.Join(m.chatLines, "\n"))
	if !strings.Contains(joined, "not available for the current model") {
		t.Fatalf("missing rejection line:\n%s", joined)
	}

	// No staged level (inline /models path): nothing applied.
	m.agent.ThinkingLevel = ""
	m.handleModelSwitchMsg(modelSwitchMsg{agent: m.agent, out: "Switched to model: m1"})
	if m.agent.ThinkingLevel != "" {
		t.Fatalf("level changed without staging: %q", m.agent.ThinkingLevel)
	}
}

func TestModelsModalMouse(t *testing.T) {
	p := &modelsTestProvider{
		current: "m1",
		catalog: []llm.ModelInfo{{ID: "m1", Current: true}, {ID: "m2"}},
		efforts: map[string][]string{"m1": {"low", "high"}, "m2": {"medium"}},
	}
	m := &Model{
		agent:       &agent.Agent{Provider: p},
		modelList:   p.catalog,
		thinkingSel: agent.ThinkingOff,
		modal:       ModalModels,
		width:       100,
		height:      40,
	}
	// Recover the overlay's centered origin the same way the handler does.
	modal := m.renderModelsModal()
	ox := (m.width - lipgloss.Width(modal)) / 2
	oy := (m.height - lipgloss.Height(modal)) / 2

	// Click the [H] chip (m1's levels off/low/high -> [Off] [L] [H]).
	levels, _ := m.modelsModalThinkingLevels()
	_, spans := buildThinkingChipRow(levels, thinkingChipCursor(levels, m.thinkingSel))
	hChip := spans[len(spans)-1]
	if !m.handleModelsModalMouse(mouseEvent{x: ox + 3 + hChip.x0, y: oy + 3, button: tea.MouseLeft, kind: mousePress}) {
		t.Fatal("chip click not consumed")
	}
	if m.thinkingSel != agent.ThinkingHigh {
		t.Fatalf("chip click staged %q, want high", m.thinkingSel)
	}

	// Click the second model row: the cursor moves and the staging syncs
	// (m2 accepts only medium, so the staged high resets to off).
	_, modelRowAt := m.modelsModalContent()
	rowY, found := 0, false
	for y, idx := range modelRowAt {
		if idx == 1 {
			rowY, found = y, true
		}
	}
	if !found {
		t.Fatal("model row for index 1 not found")
	}
	if !m.handleModelsModalMouse(mouseEvent{x: ox + 5, y: oy + 1 + rowY, button: tea.MouseLeft, kind: mousePress}) {
		t.Fatal("model row click not consumed")
	}
	if m.modelCursor != 1 {
		t.Fatalf("model row click did not move the cursor: %d", m.modelCursor)
	}
	if m.thinkingSel != agent.ThinkingOff {
		t.Fatalf("staging not synced after row click: %q", m.thinkingSel)
	}

	// A click outside the box falls through to the other handlers.
	if m.handleModelsModalMouse(mouseEvent{x: 0, y: 0, button: tea.MouseLeft, kind: mousePress}) {
		t.Fatal("click outside the box was consumed")
	}
	// Non-press events are ignored.
	if m.handleModelsModalMouse(mouseEvent{x: ox + 5, y: oy + 1 + rowY, button: tea.MouseNone, kind: mouseMotion}) {
		t.Fatal("motion event was consumed")
	}
}

// Regression: the help modal padded its key column with %-24s, which
// measures the STYLED string (escape runes included) — with colors on the
// pad never applied and the description column started right after the
// key. The column is padded by visible width now, so every description
// starts at the same column.
func TestHelpModalKeyColumnAligned(t *testing.T) {
	m := newSidebarTestModel(120)
	m.SetSize(120, 60)
	m.modal = ModalHelp

	descs := []string{
		"Submit input", "Cancel turn", "Line start", "Kill to end",
		"Scroll line", "Focus / resume", "Show this help",
	}
	cols := map[int]int{}
	for _, l := range strings.Split(stripANSI(m.renderHelpModal()), "\n") {
		for _, d := range descs {
			if i := strings.Index(l, d); i >= 0 {
				// Compare CELL columns (byte indices skew on multibyte
				// keys like "↑ ↓ j k").
				cols[ansi.StringWidth(l[:i])]++
			}
		}
	}
	if len(cols) != 1 {
		t.Fatalf("help key column is ragged: descriptions start at cells %v", cols)
	}
}
