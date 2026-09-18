package agent

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"gogen/internal/llm"
)

// isModelUnverified reports whether the selected model came from a session
// restore and has not yet been confirmed to exist at the provider. Guarded
// by statsMu (leaf lock: short critical section, no I/O).
func (a *Agent) isModelUnverified() bool {
	a.statsMu.RLock()
	defer a.statsMu.RUnlock()
	return a.modelUnverified
}

func (a *Agent) setModelUnverified(v bool) {
	a.statsMu.Lock()
	a.modelUnverified = v
	a.statsMu.Unlock()
}

// AdoptModel switches the session's provider to model and marks it
// unverified, mirroring the restore contract (RestoreSessionLocal): the
// model came from outside this session's provider — e.g. the web /new
// pane-model inheritance — so it must be confirmed to exist at the endpoint
// before the first turn sends it. The async confirmation is the caller's
// job (ValidateRestoredModel); requireModelSelected re-checks a
// still-unverified model on the first turn as a safety net.
func (a *Agent) AdoptModel(model string) {
	_ = a.Provider.SetModel(model)
	a.setModelUnverified(true)
}

func (a *Agent) CurrentModel() string {
	return a.Provider.ModelName()
}

// CurrentModelRef returns the current model's routing key (owning provider
// profile + bare wire ID) from the session's provider. Providers without
// the ModelRefProvider capability (test stubs) report the default profile.
func (a *Agent) CurrentModelRef() llm.ModelRef {
	if p, ok := a.Provider.(llm.ModelRefProvider); ok {
		return p.CurrentModelRef()
	}
	return llm.ModelRef{ID: a.Provider.ModelName()}
}

func (a *Agent) requireModelSelected(ctx context.Context) error {
	if a.Provider.ModelName() != "" && !a.isModelUnverified() {
		return nil
	}
	if a.Context != nil {
		a.Context.EnsureContextLimit(ctx)
	}
	if a.Provider.ModelName() == "" {
		// EnsureContextLimit short-circuits once the context limit is
		// resolved (e.g. pre-warmed from a session snapshot), so the
		// provider's sole-model auto-select probe may never have run. Probe
		// directly: ModelContextLimit performs sole-model auto-select and
		// is internally bounded (modelsLimitLookupTimeout) and
		// failure-tolerant (defaults on error).
		_, _ = a.Provider.ModelContextLimit(ctx)
		if a.Provider.ModelName() != "" {
			// A model picked by sole-model auto-select comes straight from
			// the provider's own list, so it is verified by construction.
			a.setModelUnverified(false)
		}
	}
	if a.Provider.ModelName() != "" && a.isModelUnverified() {
		// The selected model came from a session restore whose async
		// validation has not confirmed it yet (still running, or the
		// catalog fetch failed at startup). Re-check it here — bounded and
		// failure-tolerant — so a model that no longer exists on the
		// current provider is never sent to the endpoint.
		a.recheckRestoredModel(ctx)
	}
	if a.Provider.ModelName() != "" {
		return nil
	}
	return fmt.Errorf("no model selected; use /models to list and choose a model")
}

// recheckRestoredModel confirms the selected model still exists at the
// provider, clearing it (with sole-model auto-select fallback) when it is
// gone. On a catalog lookup error the model is kept — the endpoint may be
// temporarily unreachable — and stays marked unverified so a later turn
// re-checks.
func (a *Agent) recheckRestoredModel(ctx context.Context) {
	model := a.Provider.ModelName()
	models, err := a.Provider.ListModels(ctx)
	if err != nil {
		return // keep the model, keep it unverified (fail-open)
	}
	if slices.ContainsFunc(models, func(m llm.ModelInfo) bool { return m.ID == model }) {
		a.setModelUnverified(false)
		return
	}
	// Confirmed absent: clear the stale model — unless a concurrent
	// /models selection replaced it in the meantime — then let
	// sole-model auto-select fill the gap if this provider serves exactly
	// one model.
	if a.Provider.ModelName() == model {
		_ = a.Provider.SetModel("")
	}
	a.setModelUnverified(false)
	if a.Provider.ModelName() == "" {
		_, _ = a.Provider.ModelContextLimit(ctx)
	}
}

func (a *Agent) ContextLimit() int {
	if a.Context == nil {
		return 0
	}
	return a.Context.ContextLimit()
}

func (a *Agent) ListModels(ctx context.Context) ([]llm.ModelInfo, error) {
	models, err := a.Provider.ListModels(ctx)
	if err != nil {
		return nil, err
	}
	// The current flag is per (profile, ID): the same model ID listed by
	// several providers marks only the copy the session actually runs on.
	ref := a.CurrentModelRef()
	for i := range models {
		models[i].Current = ref.Valid() && models[i].ID == ref.ID &&
			(models[i].Provider == "" || models[i].Provider == ref.ProfileName())
	}
	return models, nil
}

func (a *Agent) SelectModel(ctx context.Context, selector string) error {
	selector = strings.TrimSpace(selector)
	if selector == "" {
		return fmt.Errorf("model selector is required")
	}

	models, err := a.ListModels(ctx)
	if err != nil {
		return err
	}
	if len(models) == 0 {
		return fmt.Errorf("no models available from the endpoint")
	}

	// The selection resolves to a (profile, ID) pair: model IDs are not
	// unique across registered providers, so the owning profile travels
	// with the selection (the picker sends "profile/id" qualified
	// selectors for non-default groups; bare selectors resolve to the
	// first catalog entry that matches, profile order).
	var modelID, profile string
	if n, err := strconv.Atoi(selector); err == nil {
		if n < 1 || n > len(models) {
			return fmt.Errorf("invalid model number %d (1-%d)", n, len(models))
		}
		modelID = models[n-1].ID
		profile = models[n-1].Provider
	} else {
		// Qualified selector first: an entry whose own "provider/id" form
		// equals the selector (no splitting — model IDs may contain "/").
		for _, m := range models {
			if m.Provider != "" && m.Provider != llm.DefaultProfileName && m.Provider+"/"+m.ID == selector {
				modelID = m.ID
				profile = m.Provider
				break
			}
		}
		if modelID == "" {
			for _, m := range models {
				if m.ID == selector {
					modelID = m.ID
					profile = m.Provider
					break
				}
			}
		}
		if modelID == "" {
			for _, m := range models {
				if strings.Contains(m.ID, selector) {
					modelID = m.ID
					profile = m.Provider
					break
				}
			}
		}
		if modelID == "" {
			return fmt.Errorf("model not found: %q", selector)
		}
	}

	// SetModelRef sets the routing key (profile + wire ID) in one step so
	// they cannot drift; providers without the capability (test stubs)
	// treat every model as belonging to the default profile.
	if p, ok := a.Provider.(llm.ModelRefProvider); ok {
		if err := p.SetModelRef(llm.ModelRef{Profile: profile, ID: modelID}); err != nil {
			return err
		}
	} else if err := a.Provider.SetModel(modelID); err != nil {
		return err
	}
	// A selection always comes from the provider's own model list, so it is
	// verified by construction.
	a.setModelUnverified(false)
	if a.Context != nil {
		a.Context.RefreshAfterModelChange(ctx)
	}
	a.clearTurnUsage()
	return nil
}

// ParseModelsCommand reports whether input is a /models command.
// If selectArg is non-empty, the user is selecting a model; otherwise it is list-only.
func ParseModelsCommand(input string) (selectArg string, ok bool) {
	trimmed := strings.TrimSpace(input)
	if trimmed != "/models" && trimmed != "models" && !strings.HasPrefix(trimmed, "/models ") && !strings.HasPrefix(trimmed, "models ") {
		return "", false
	}
	arg := strings.TrimSpace(strings.TrimPrefix(strings.TrimPrefix(trimmed, "/models"), "models"))
	return arg, true
}

// HandleModelsCommand processes /models and /models <selector>.
// Returns output text and whether the command was handled.
func (a *Agent) HandleModelsCommand(ctx context.Context, input string) (string, bool, error) {
	arg, ok := ParseModelsCommand(input)
	if !ok {
		return "", false, nil
	}

	if arg != "" {
		if err := a.SelectModel(ctx, arg); err != nil {
			return "", true, err
		}
		limit := a.ContextLimit()
		if limit > 0 {
			return fmt.Sprintf("Switched to model: %s (context: %d tokens)", a.CurrentModel(), limit), true, nil
		}
		return fmt.Sprintf("Switched to model: %s", a.CurrentModel()), true, nil
	}

	models, err := a.ListModels(ctx)
	if err != nil {
		return "", true, err
	}
	if len(models) == 0 {
		return "No models reported by the endpoint.", true, nil
	}
	if len(models) == 1 {
		m := models[0]
		limit := m.ContextLimit
		if limit > 0 {
			return fmt.Sprintf("Single model available: %s (n_ctx=%d)", m.ID, limit), true, nil
		}
		return fmt.Sprintf("Single model available: %s", m.ID), true, nil
	}

	var b strings.Builder
	b.WriteString("Available models (* = current):\n")
	for i, m := range models {
		marker := " "
		if m.Current {
			marker = "*"
		}
		if m.ContextLimit > 0 {
			fmt.Fprintf(&b, "  %2d. %s  (n_ctx=%d) %s\n", i+1, m.ID, m.ContextLimit, marker)
		} else {
			fmt.Fprintf(&b, "  %2d. %s %s\n", i+1, m.ID, marker)
		}
	}
	b.WriteString("\nUse /models <number> or /models <name> to switch.")
	return b.String(), true, nil
}
