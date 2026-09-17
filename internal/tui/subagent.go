package tui

import (
	"context"

	"gogen/internal/agent"
	"gogen/internal/config"
	"gogen/internal/llm"
	"gogen/internal/session"
)

// subagentRecord is one finished nested session shown by /subagents.
type subagentRecord struct {
	label  string
	report string
	err    error
}

// tuiSubagentSpawner runs nested sessions for the TUI: a fresh agent over a
// fresh provider, sharing the parent's executor, store, and project context
// (D9 — the shared session factory). Children are ephemeral (no registry,
// no panes); their final reports land in the parent's tool result AND in
// the /subagents modal list. The existing turn spinner is the running
// indicator; nesting follows subagent_max_depth (default 1 = children
// cannot spawn).
type tuiSubagentSpawner struct {
	cfg *config.Config
	m   *Model // for the /subagents list (nil in tests)

	// providerFactory builds the child provider; nil = the OpenAI
	// with-profiles default (tests inject a mock).
	providerFactory func(cfg *config.Config, parent *agent.Agent) llm.LLMProvider
}

func (sp *tuiSubagentSpawner) Spawn(ctx context.Context, parent *agent.Agent, job, model string, depth int) (string, error) {
	cfg := sp.cfg
	// The child must inherit the PARENT's current model, not the startup
	// config value: the user may have switched models at runtime (/model),
	// or the model may only have been selected interactively (cfg
	// OpenAIModel empty) — a child seeded from cfg would fail
	// requireModelSelected or run a different model than its parent. The
	// provider working dir follows the parent's live working dir too. An
	// explicit model argument on the subagent tool wins over inheritance.
	// The child carries ALL registered OpenAI-compatible profiles (the
	// legacy fields form the implicit default), like the parent: a model
	// inherited from a secondary provider must route to its owning endpoint,
	// not fall back to the default client (D9).
	var prov llm.LLMProvider
	if sp.providerFactory != nil {
		prov = sp.providerFactory(cfg, parent)
	} else {
		openaiProv := llm.NewOpenAIProviderWithProfiles(
			llm.ProviderProfiles(cfg.OpenAIKey, cfg.OpenAIModel, cfg.OpenAIURL, cfg.OpenAIProviders),
			cfg.OpenAIModel, parent.WorkingDir, nil)
		// Share the parent's model→profile owner record: a child provider
		// whose owning endpoint is down still routes inherited models to
		// that endpoint instead of falling back to the default profile
		// (the child's own catalog merge cannot map the model while the
		// owner is unreachable).
		if pp, ok := parent.Provider.(*llm.OpenAIProvider); ok {
			openaiProv.SetOwnerRegistry(pp.OwnerRegistry())
			// Share the parent's per-endpoint catalog cache too: a child
			// must not re-download the catalog the parent already holds.
			openaiProv.SetEndpointCatalog(pp.EndpointCatalog())
			openaiProv.SetPropsCapsCache(pp.PropsCapsCache())
		}
		prov = openaiProv
	}
	child := agent.NewSessionAgent(agent.SessionAgentOptions{
		Provider:              prov,
		Executor:              parent.Executor,
		Store:                 parent.SessionStore,
		Config:                cfg,
		GlobalMode:            parent.GlobalMode,
		ProjectFilePath:       parent.ProjectFilePath,
		ProjectGuidelines:     parent.ProjectGuidelines,
		TestCommand:           parent.TestCommand,
		LintCommand:           parent.LintCommand,
		WorkingDir:            parent.WorkingDir,
		MCPRegistry:           parent.MCPRegistry,
		DebugCompareMessages:  parent.DebugCompareMessages,
		BoardEnabled:          parent.BoardEnabled(),
		SubagentsEnabled:      parent.SubagentsEnabled(),
		SubagentMaxDepth:      parent.SubagentMaxDepth(),
		SubagentMaxConcurrent: parent.SubagentMaxConcurrent(),
		BoardManager:          parent.BoardManager(),
		SkillsManager:         parent.SkillsManager(),
		TerminalEnabled:       parent.TerminalEnabled(),
		InstructionsEnabled:   parent.InstructionsEnabled(),
		SubagentSpawner:       sp, // nesting allowed up to the configured depth
	}, nil, session.NewID())
	child.SetSubagentDepth(depth + 1)
	child.SetParentID(parent.SessionID)
	// Model cascade (fully shared with the web spawner — see
	// ResolveSubagentModel/ApplySubagentModel): explicit tool argument >
	// configured subagent model > the parent's live model (unless it is
	// the default the child is already seeded with). The explicit
	// argument is a hard requirement and fails the spawn when
	// unselectable; the other tiers fall back to the seeded default
	// (fail open).
	m, src := agent.ResolveSubagentModel(model, cfg.SubagentModel, parent.CurrentModel(), cfg.OpenAIModel)
	if err := agent.ApplySubagentModel(ctx, child, m, src); err != nil {
		return "", err
	}
	// Reasoning effort: the same cascade as the web spawner — the
	// configured subagent level (subagent_thinking_level) wins; empty =
	// inherit the parent's live level. A level the child's final model
	// does not accept is omitted (shared helper, so the hosts cannot
	// drift).
	agent.ApplySubagentThinkingLevel(child, parent, cfg.SubagentThinkingLevel)
	label := agent.SubagentLabel(job)
	_, _ = child.RenameSession(label)
	// The job wrapper applies AFTER label derivation, so the /subagents
	// list keeps the original job the parent wrote; only the child's first
	// message carries the wrapped job.
	report, err := child.StreamProcessInputWithImages(ctx, agent.FormatSubagentJob(job), nil, nil)
	// Persist the final outcome on the child's snapshot (shared with the
	// web spawner — FinishSubagentOutcome): the sidebar reads it from the
	// sessions payload when the live subagent events are gone.
	child.FinishSubagentOutcome(report, err)
	if sp.m != nil {
		sp.m.recordSubagent(subagentRecord{label: label, report: agent.TruncateSubagentReport(report), err: err})
	}
	return report, err
}
