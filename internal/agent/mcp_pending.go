package agent

import (
	"context"
	"fmt"
	"sync"
	"time"

	"gogen/internal/llm"
)

// MCPRegistryWaiter is a registry whose tools may still be connecting.
// mcpRegistryOrError waits on it so a gateway call made during the handshake
// blocks until the servers attach or the init budget runs out, instead of
// failing immediately.
type MCPRegistryWaiter interface {
	Wait(ctx context.Context) error
}

// handshakeWaitSlack is added on top of the configured init budget so a
// handshake that finishes just as its own deadline fires can publish the
// real error before the waiter reports a generic timeout.
const handshakeWaitSlack = 5 * time.Second

// PendingMCPRegistry is the registry every session shares while MCP servers
// are still starting.
//
// startMCP installs this pointer before the handshake goroutine runs, and the
// web workspace copies that same pointer into every later session. Publish
// fills it in place. Replacing the agent's registry field after the copy
// would leave those sessions nil forever — which is what "configured but none
// finished its handshake" kept reporting long after the server was up.
type PendingMCPRegistry struct {
	wait time.Duration

	mu       sync.RWMutex
	inner    MCPToolRegistry
	err      error
	finished bool
	done     chan struct{}
}

// NewPendingMCPRegistry waits up to the servers' init budget (plus a little
// slack) for Publish or Fail. A non-positive budget falls back to one minute,
// matching the client's default handshake bound.
func NewPendingMCPRegistry(wait time.Duration) *PendingMCPRegistry {
	if wait <= 0 {
		wait = time.Minute
	}
	return &PendingMCPRegistry{
		wait: wait + handshakeWaitSlack,
		done: make(chan struct{}),
	}
}

// Publish attaches the connected registry. Later Wait calls return nil and
// Definitions/CallTool delegate to reg. A second call is ignored.
func (p *PendingMCPRegistry) Publish(reg MCPToolRegistry) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	p.inner = reg
	p.finished = true
	close(p.done)
}

// Fail records why no server connected. Later Wait calls return err.
func (p *PendingMCPRegistry) Fail(err error) {
	if p == nil {
		return
	}
	if err == nil {
		err = fmt.Errorf("MCP handshake failed")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.finished {
		return
	}
	p.err = err
	p.finished = true
	close(p.done)
}

// Wait blocks until Publish, Fail, ctx cancellation, or the init budget.
// A handshake that finished before Wait, or in the same instant as the
// deadline, reports that result rather than the timeout.
func (p *PendingMCPRegistry) Wait(ctx context.Context) error {
	if p == nil {
		return fmt.Errorf("MCP registry is not configured")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err, done := p.outcome(); done {
		return err
	}
	timer := time.NewTimer(p.wait)
	defer timer.Stop()
	select {
	case <-p.done:
		err, _ := p.outcome()
		return err
	case <-ctx.Done():
		if err, done := p.outcome(); done {
			return err
		}
		return fmt.Errorf("MCP handshake interrupted: %w", ctx.Err())
	case <-timer.C:
		if err, done := p.outcome(); done {
			return err
		}
		return fmt.Errorf("MCP handshake did not finish within %s; raise mcp_servers[].init_timeout_secs if the launcher is slow", p.wait)
	}
}

func (p *PendingMCPRegistry) outcome() (error, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.err, p.finished
}

// Definitions returns the connected tools, or nil while the handshake is
// still running. It does not wait: prompt construction calls it, and blocking
// there would stall every turn for the whole init budget.
func (p *PendingMCPRegistry) Definitions() []llm.Tool {
	reg := p.current()
	if reg == nil {
		return nil
	}
	return reg.Definitions()
}

// ToolNames returns the connected tool names, or nil before the handshake.
func (p *PendingMCPRegistry) ToolNames() map[string]struct{} {
	reg := p.current()
	if reg == nil {
		return nil
	}
	return reg.ToolNames()
}

// CallTool waits for the handshake, then forwards the call.
func (p *PendingMCPRegistry) CallTool(ctx context.Context, name string, args map[string]any) (string, error) {
	if err := p.Wait(ctx); err != nil {
		return "", err
	}
	reg := p.current()
	if reg == nil {
		return "", fmt.Errorf("MCP registry is not configured")
	}
	return reg.CallTool(ctx, name, args)
}

func (p *PendingMCPRegistry) current() MCPToolRegistry {
	if p == nil {
		return nil
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	return p.inner
}
