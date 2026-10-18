package server

import (
	"context"
	"sync"
	"testing"
	"time"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/contextmgr"
	"gogen/internal/llm"
)

// probeStreamStub streams turns with a short delay so reader goroutines
// overlap the turn's message appends. The first LLM round of each turn
// returns a tool call (so the assistant message carries ToolCalls and the
// next round's in-place stabilization writes ArgsStabilized on the live
// array); the second round returns plain content, ending the turn.
type probeStreamStub struct {
	mu    sync.Mutex
	calls int
}

func (p *probeStreamStub) GenerateResponse(context.Context, []llm.Message, map[string]struct{}, []llm.Tool) (llm.Response, error) {
	return llm.Response{Content: "summary"}, nil
}

func (p *probeStreamStub) GenerateResponseStream(_ context.Context, _ []llm.Message, _ map[string]struct{}, _ []llm.Tool, h *llm.StreamHandlers) (*llm.StreamResult, error) {
	p.mu.Lock()
	p.calls++
	toolRound := p.calls%2 == 1
	p.mu.Unlock()
	time.Sleep(10 * time.Millisecond)
	if h != nil && h.OnStreamOpened != nil {
		h.OnStreamOpened()
	}
	usage := &llm.Usage{PromptTokens: 10, CompletionTokens: 2, TotalTokens: 12}
	if toolRound {
		return &llm.StreamResult{
			ToolCalls: []llm.ToolCall{{
				ID:   "c1",
				Name: "read_file",
				Args: map[string]any{"path": "a.go"},
			}},
			Usage: usage,
		}, nil
	}
	return &llm.StreamResult{Content: "ok", Usage: usage}, nil
}

func (p *probeStreamStub) ModelContextLimit(context.Context) (int, error) { return 1000, nil }
func (p *probeStreamStub) ListModels(context.Context) ([]llm.ModelInfo, error) {
	return []llm.ModelInfo{{ID: "probe", ContextLimit: 1000, Current: true}}, nil
}
func (p *probeStreamStub) SetModel(string) error   { return nil }
func (p *probeStreamStub) ModelName() string       { return "probe" }
func (p *probeStreamStub) SetThinkingLevel(string) {}

// TestServerConfigProbeConcurrentWithTurn verifies the web server's config
// probe path — agentConfigMsg (which calls ContextStats and reads usage
// totals without the turn lock) and SnapshotMessages — is safe while a turn
// goroutine appends messages, accumulates usage, and stabilizes tool args.
// Run with -race to catch regressions.
func TestServerConfigProbeConcurrentWithTurn(t *testing.T) {
	provider := &probeStreamStub{}
	ctxMgr := contextmgr.NewManager(provider, contextmgr.Settings{ContextLimit: 1000})
	a := agent.NewAgent(provider, agent.NewExecutor(t.TempDir()), ctxMgr)
	s := newTestServer(t, a, &config.Config{})
	rt := s.registry.first()

	var wg sync.WaitGroup
	stop := make(chan struct{})
	for i := 0; i < 3; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = agentConfigMsg(context.Background(), rt)
					_ = rt.agent.SnapshotMessages()
				}
			}
		}()
	}

	for i := 0; i < 15; i++ {
		if _, err := a.StreamProcessInput(context.Background(), "hi", nil); err != nil {
			t.Fatalf("StreamProcessInput: %v", err)
		}
	}
	close(stop)
	wg.Wait()
}
