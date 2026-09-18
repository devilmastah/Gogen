package llmtest

import (
	"context"
	"sync"

	"gogen/internal/llm"
)

// MockProvider is a configurable llm.LLMProvider for tests.
type MockProvider struct {
	mu sync.Mutex

	Model string
	// ModelProfile is the registered provider profile owning Model (the
	// ModelRef routing key; "" = the default profile). SetModel resets it
	// to "" exactly like the real provider (a bare ID cannot name a
	// non-default owner); SetModelRef sets both together.
	ModelProfile  string
	ContextLimit  int
	Models        []llm.ModelInfo
	Responses     []llm.Response // consumed in order by GenerateResponse
	StreamResults []*llm.StreamResult
	GenerateErr   error
	StreamErr     error
	SetModelErr   error
	CallCount     int
	LastMessages  []llm.Message
	LastAllowed   map[string]struct{}
	LastTools     []llm.Tool
	// ThinkingLevel records the last SetThinkingLevel call ("" = never
	// set) so spawn-time wiring tests can assert the level reached the
	// child provider.
	ThinkingLevel string
	OnGenerate    func(ctx context.Context, messages []llm.Message) (llm.Response, error)
	OnStream      func(ctx context.Context, messages []llm.Message, h *llm.StreamHandlers) (*llm.StreamResult, error)
}

// NewMockProvider returns a mock with sensible defaults.
func NewMockProvider() *MockProvider {
	return &MockProvider{
		Model:        "mock-model",
		ContextLimit: 128000,
		Models:       []llm.ModelInfo{{ID: "mock-model", ContextLimit: 128000, Current: true}},
		Responses:    []llm.Response{{Content: "ok"}},
	}
}

func (m *MockProvider) GenerateResponse(ctx context.Context, messages []llm.Message, allowedTools map[string]struct{}, tools []llm.Tool) (llm.Response, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.CallCount++
	m.LastMessages = append([]llm.Message(nil), messages...)
	m.LastAllowed = allowedTools
	m.LastTools = append([]llm.Tool(nil), tools...)
	if m.OnGenerate != nil {
		return m.OnGenerate(ctx, messages)
	}
	if m.GenerateErr != nil {
		return llm.Response{}, m.GenerateErr
	}
	if len(m.Responses) == 0 {
		return llm.Response{Content: "ok"}, nil
	}
	resp := m.Responses[0]
	if len(m.Responses) > 1 {
		m.Responses = m.Responses[1:]
	}
	return resp, nil
}

func (m *MockProvider) GenerateResponseStream(ctx context.Context, messages []llm.Message, allowedTools map[string]struct{}, extraTools []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	m.mu.Lock()
	onStream := m.OnStream
	streamErr := m.StreamErr
	var result *llm.StreamResult
	if len(m.StreamResults) > 0 {
		result = m.StreamResults[0]
		if len(m.StreamResults) > 1 {
			m.StreamResults = m.StreamResults[1:]
		}
	}
	m.mu.Unlock()

	if onStream != nil {
		return onStream(ctx, messages, h)
	}
	if streamErr != nil {
		return nil, streamErr
	}

	resp, err := m.GenerateResponse(ctx, messages, allowedTools, extraTools)
	if err != nil {
		return nil, err
	}
	if result != nil {
		if h != nil {
			emitStreamResult(h, result)
		}
		return result, nil
	}
	out := &llm.StreamResult{Content: resp.Content, Reasoning: resp.Reasoning, Refusal: resp.Refusal, ToolCalls: resp.ToolCalls, Usage: resp.Usage}
	if h != nil {
		emitStreamResult(h, out)
	}
	return out, nil
}

func emitStreamResult(h *llm.StreamHandlers, result *llm.StreamResult) {
	if h.OnStart != nil {
		h.OnStart()
	}
	if h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	if result.Reasoning != "" && h.OnThinkingToken != nil {
		h.OnThinkingToken(result.Reasoning)
	}
	if result.Content != "" && h.OnToken != nil {
		h.OnToken(result.Content)
	} else if result.Refusal != "" && h.OnToken != nil {
		h.OnToken(result.Refusal)
	}
	for _, tc := range result.ToolCalls {
		if h.OnToolCallStart != nil {
			h.OnToolCallStart(tc.Index, tc.ID, tc.Name)
		}
		if h.OnToolCall != nil {
			h.OnToolCall(tc)
		}
	}
	if h.OnStreamEnd != nil && len(result.ToolCalls) > 0 {
		h.OnStreamEnd()
	}
}

func (m *MockProvider) ModelContextLimit(context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.ContextLimit > 0 {
		return m.ContextLimit, nil
	}
	return 128000, nil
}

func (m *MockProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Models != nil {
		return append([]llm.ModelInfo(nil), m.Models...), nil
	}
	return []llm.ModelInfo{{ID: m.ModelName(), Current: true}}, nil
}

func (m *MockProvider) SetModel(id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SetModelErr != nil {
		return m.SetModelErr
	}
	m.Model = id
	// A bare ID cannot name a non-default owner: the routing key resets to
	// the default profile (the real provider's contract).
	m.ModelProfile = ""
	return nil
}

// SetModelRef selects the model AND its owning profile in one step (the
// ModelRefProvider capability).
func (m *MockProvider) SetModelRef(ref llm.ModelRef) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.SetModelErr != nil {
		return m.SetModelErr
	}
	m.Model = ref.ID
	m.ModelProfile = ref.Profile
	return nil
}

// CurrentModelRef returns the current model's routing key (the
// ModelRefProvider capability).
func (m *MockProvider) CurrentModelRef() llm.ModelRef {
	m.mu.Lock()
	defer m.mu.Unlock()
	return llm.ModelRef{Profile: m.ModelProfile, ID: m.Model}
}

func (m *MockProvider) ModelName() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Model == "" {
		return "mock-model"
	}
	return m.Model
}

func (m *MockProvider) SetThinkingLevel(level string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ThinkingLevel = level
}

// Ensure MockProvider implements llm.LLMProvider.
var _ llm.LLMProvider = (*MockProvider)(nil)
