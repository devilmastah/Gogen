package agent

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gogen/internal/llm"
)

// slowListProvider delays ListModels so two validations on one agent overlap
// deterministically: the second installs OnModelChanged while the first is
// still inside the provider call, and the first reads the hook when it lands.
type slowListProvider struct {
	countingProvider
	delay time.Duration
}

func (p *slowListProvider) ListModels(context.Context) ([]llm.ModelInfo, error) {
	time.Sleep(p.delay)
	return nil, nil
}

// TestValidateRestoredModelHookInstallIsSerialized races an install of
// OnModelChanged against a validation goroutine's read of it. Before
// onModelChangedMu serialized the install/read pair, this test failed under
// -race (write in ValidateRestoredModelAsync vs read at the end of
// ValidateRestoredModel).
func TestValidateRestoredModelHookInstallIsSerialized(t *testing.T) {
	p := &slowListProvider{countingProvider: countingProvider{limit: 20000}, delay: 40 * time.Millisecond}
	a := NewAgent(p, &Executor{WorkingDir: "."}, nil)

	var validationDone sync.WaitGroup
	validationDone.Add(1)
	go func() {
		defer validationDone.Done()
		a.ValidateRestoredModel(context.Background(), "m")
	}()

	// Install a hook while the first validation is inside ListModels.
	time.Sleep(5 * time.Millisecond)
	var hookCalls atomic.Int32
	hookFired := make(chan struct{}, 1)
	a.ValidateRestoredModelAsync("m", func() {
		hookCalls.Add(1)
		select {
		case hookFired <- struct{}{}:
		default:
		}
	})

	validationDone.Wait()
	select {
	case <-hookFired:
	case <-time.After(2 * time.Second):
		t.Fatal("installed hook never fired")
	}
	if hookCalls.Load() == 0 {
		t.Fatal("hook was installed but never invoked")
	}
}
