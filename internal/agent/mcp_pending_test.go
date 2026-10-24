package agent

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPendingMCPRegistryListWaitsForPublish(t *testing.T) {
	pending := NewPendingMCPRegistry(2 * time.Second)
	a := newGatewayAgent(t, true, true, pending)

	type result struct {
		out string
		err error
	}
	ch := make(chan result, 1)
	go func() {
		out, err := handleMCP(context.Background(), a, map[string]any{"action": "list"})
		ch <- result{out, err}
	}()

	time.Sleep(40 * time.Millisecond)
	select {
	case res := <-ch:
		t.Fatalf("list returned before publish: %q %v", res.out, res.err)
	default:
	}

	pending.Publish(gatewayRegistry())

	select {
	case res := <-ch:
		if res.err != nil {
			t.Fatal(res.err)
		}
		if !strings.Contains(res.out, "mcp_rea_analyze_binary") {
			t.Fatalf("list = %q, want the published tool", res.out)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("list did not return after publish")
	}
}

func TestPendingMCPRegistryFailSurfacesToGateway(t *testing.T) {
	pending := NewPendingMCPRegistry(2 * time.Second)
	a := newGatewayAgent(t, true, true, pending)
	pending.Fail(errors.New("rea failed to start: node not found"))

	_, err := handleMCP(context.Background(), a, map[string]any{"action": "list"})
	if err == nil || !strings.Contains(err.Error(), "node not found") {
		t.Fatalf("list error = %v, want the handshake failure", err)
	}
}

func TestPendingMCPRegistryWaitHonorsDeadline(t *testing.T) {
	pending := NewPendingMCPRegistry(time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	err := pending.Wait(ctx)
	if err == nil || !strings.Contains(err.Error(), "interrupted") {
		t.Fatalf("Wait error = %v, want the caller deadline", err)
	}
}
