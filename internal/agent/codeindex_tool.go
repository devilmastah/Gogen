package agent

import (
	"context"
	"fmt"
	"strings"

	"gogen/internal/codeindex"
	"gogen/internal/config"
	"gogen/internal/llm"
)

// searchMeaningDescription is the model-facing description. Decision 6 of
// the code index plan is the source of this paragraph.
const searchMeaningDescription = "Search the codebase by meaning, not by exact text. Use it when you do not know the symbol or the file name, or to see how a behavior is implemented across the codebase. Use search_code when you know the exact text. Write a short query with concrete words, and set path to a subdirectory to narrow it. Building the index sends file text to the embedding provider."

func searchMeaningToolDef() llm.Tool {
	return toolDef("search_meaning", searchMeaningDescription,
		toolSchema(
			map[string]any{
				"query": toolProp("string", "Short query with concrete words"),
				"path":  toolProp("string", "Optional subdirectory of the working directory"),
			},
			"query",
		),
	)
}

// SetCodeIndexEnabled toggles search_meaning. Off registers no tool.
func (a *Agent) SetCodeIndexEnabled(on bool) {
	a.flags().SetCodeIndexEnabled(on)
}

// CodeIndexEnabled reports whether search_meaning is registered.
func (a *Agent) CodeIndexEnabled() bool {
	return a.flags().CodeIndexEnabled()
}

// SetCodeIndex installs the shared index for this working folder. The
// pointer is shared by every session, the same way the board is. Nil clears
// it. The index lock is not the agent lock: callers take it only inside
// Search and Rebuild, and the progress callback must not call back into the
// agent.
func (a *Agent) SetCodeIndex(ix *codeindex.Index) {
	a.codeIndex.Store(ix)
}

// CodeIndex returns the shared index, or nil when none was installed.
func (a *Agent) CodeIndex() *codeindex.Index {
	return a.codeIndex.Load()
}

// SetCodeIndexConfig installs the live config read at each search. The web
// host passes the runtime overlay with the live provider list. A nil func
// reads nothing.
func (a *Agent) SetCodeIndexConfig(fn func() config.Config) {
	if fn == nil {
		a.codeIndexConfig.Store(codeIndexConfig(nil))
		return
	}
	a.codeIndexConfig.Store(codeIndexConfig(fn))
}

// SetCodeIndexEmbedder installs a test embedder. Production leaves it nil
// and posts to the provider.
func (a *Agent) SetCodeIndexEmbedder(e codeindex.Embedder) {
	if e == nil {
		a.codeIndexEmbedder.Store(codeIndexEmbedder(nil))
		return
	}
	a.codeIndexEmbedder.Store(codeIndexEmbedder(e))
}

type codeIndexConfig func() config.Config
type codeIndexEmbedder codeindex.Embedder

func (a *Agent) codeIndexCfg() config.Config {
	v := a.codeIndexConfig.Load()
	if v == nil {
		return config.Config{}
	}
	fn := v.(codeIndexConfig)
	if fn == nil {
		return config.Config{}
	}
	return fn()
}

func (a *Agent) ensureCodeIndex() *codeindex.Index {
	if ix := a.codeIndex.Load(); ix != nil {
		return ix
	}
	ix := codeindex.Open(a.WorkingDirSnapshot())
	if a.codeIndex.CompareAndSwap(nil, ix) {
		return ix
	}
	return a.codeIndex.Load()
}

// codeIndexProviderSet reports whether an embedding provider name is configured.
// An empty name leaves the code index off: the tool is not registered, the
// watch does not run, and nothing is embedded or written.
func codeIndexProviderSet(cfg config.Config) bool {
	return strings.TrimSpace(cfg.CodeIndexProvider) != ""
}

// codeIndexTarget resolves the embedding endpoint. The API key is returned
// beside the identity and is never written into the index. An empty
// code_index_provider is an error. A named provider with an empty model
// uses that provider's own model. The chat model is not sent on its own.
func codeIndexTarget(cfg config.Config) (codeindex.Identity, string, error) {
	lines, overlap := cfg.CodeIndexChunk()
	name := strings.TrimSpace(cfg.CodeIndexProvider)
	model := strings.TrimSpace(cfg.CodeIndexModel)
	if name == "" {
		return codeindex.Identity{}, "", fmt.Errorf("code index provider is not set")
	}
	base, key, provModel, ok := lookupEmbeddingProvider(cfg, name)
	if !ok {
		return codeindex.Identity{}, "", fmt.Errorf("code index provider %q is not configured", name)
	}
	if model == "" {
		model = strings.TrimSpace(provModel)
	}
	if model == "" {
		return codeindex.Identity{}, "", codeindex.ErrNoModel
	}
	return codeindex.Identity{
		Provider: name, BaseURL: base, Model: model,
		ChunkLines: lines, ChunkOverlap: overlap,
	}, key, nil
}

func lookupEmbeddingProvider(cfg config.Config, name string) (base, key, model string, ok bool) {
	if name == llm.DefaultProfileName {
		return cfg.OpenAIURL, cfg.OpenAIKey, cfg.OpenAIModel, true
	}
	for _, p := range cfg.OpenAIProviders {
		if p.Name == name {
			return p.BaseURL, p.APIKey, p.Model, true
		}
	}
	return "", "", "", false
}

// SetCodeIndexReranker installs a test reranker. Production leaves it nil
// and posts to the rerank provider. The reranker is used only while
// code_index_rerank is on.
func (a *Agent) SetCodeIndexReranker(r codeindex.Reranker) {
	if r == nil {
		a.codeIndexReranker.Store(codeIndexReranker(nil))
		return
	}
	a.codeIndexReranker.Store(codeIndexReranker(r))
}

type codeIndexReranker codeindex.Reranker

func (a *Agent) codeIndexRerankerFor() codeindex.Reranker {
	if !codeIndexRerankOn(a.codeIndexCfg()) {
		return nil
	}
	if v := a.codeIndexReranker.Load(); v != nil {
		if r := v.(codeIndexReranker); r != nil {
			return r
		}
	}
	base, key, model, err := codeIndexRerankTarget(a.codeIndexCfg())
	if err != nil {
		return errReranker{err}
	}
	return llmReranker{base: base, key: key, model: model}
}

type errReranker struct{ err error }

func (r errReranker) Rerank(context.Context, string, []string) ([]codeindex.RerankHit, error) {
	return nil, r.err
}

type llmReranker struct {
	base  string
	key   string
	model string
}

func (r llmReranker) Rerank(ctx context.Context, query string, documents []string) ([]codeindex.RerankHit, error) {
	rows, err := llm.PostRerank(ctx, r.base, r.key, r.model, query, documents)
	if err != nil {
		return nil, err
	}
	out := make([]codeindex.RerankHit, len(rows))
	for i, row := range rows {
		out[i] = codeindex.RerankHit{Index: row.Index, Score: row.Score}
	}
	return out, nil
}

func (a *Agent) codeIndexEmbedderFor(id codeindex.Identity, key string) codeindex.Embedder {
	if v := a.codeIndexEmbedder.Load(); v != nil {
		if e := v.(codeIndexEmbedder); e != nil {
			return e
		}
	}
	return embedderFunc(func(ctx context.Context, texts []string) ([][]float32, error) {
		return llm.PostEmbeddings(ctx, id.BaseURL, key, id.Model, texts)
	})
}

type embedderFunc func(ctx context.Context, texts []string) ([][]float32, error)

func (f embedderFunc) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	return f(ctx, texts)
}

func (a *Agent) prepareCodeIndex() (codeindex.Identity, codeindex.Embedder, error) {
	id, key, err := codeIndexTarget(a.codeIndexCfg())
	if err != nil {
		return codeindex.Identity{}, nil, err
	}
	return id, a.codeIndexEmbedderFor(id, key), nil
}

func handleSearchMeaning(ctx context.Context, a *Agent, args map[string]any) (string, error) {
	query, err := stringArg(args, "query")
	if err != nil {
		return "", err
	}
	path, err := stringArgOptional(args, "path")
	if err != nil {
		return "", err
	}
	id, emb, err := a.prepareCodeIndex()
	if err != nil {
		return "", err
	}
	return a.ensureCodeIndex().SearchIn(ctx, emb, id, query, path, a.WorkingDirSnapshot(), a.codeIndexRerankerFor())
}

// SyncCodeIndexWatch starts or stops the shared index's filesystem watch.
// The watch runs only while code_index is on, an embedding provider is set,
// and code_index_watch is not an explicit off. It does not build the first
// index. The provider name is runtime config, so this also marks the shared
// tool set stale: search_meaning is registered only while that name is set.
func (a *Agent) SyncCodeIndexWatch() {
	a.flags().gen.Add(1)
	ix := a.CodeIndex()
	if ix == nil {
		return
	}
	cfg := a.codeIndexCfg()
	if !a.CodeIndexEnabled() || !codeIndexProviderSet(cfg) || !codeIndexWatchOn(cfg) {
		ix.SetWatch(false, nil)
		return
	}
	ix.SetWatch(true, func() (codeindex.Embedder, codeindex.Identity, error) {
		cfg := a.codeIndexCfg()
		if !a.CodeIndexEnabled() || !codeIndexProviderSet(cfg) || !codeIndexWatchOn(cfg) {
			return nil, codeindex.Identity{}, nil
		}
		id, emb, err := a.prepareCodeIndex()
		return emb, id, err
	})
}

func codeIndexWatchOn(cfg config.Config) bool {
	return cfg.CodeIndexWatchEnabled()
}

func codeIndexRerankOn(cfg config.Config) bool {
	return cfg.CodeIndexRerankEnabled()
}

// codeIndexRerankTarget resolves the rerank request. An empty provider uses
// the embedding provider's base URL and key. An empty model uses a named
// rerank provider's own model. The embedding model and the chat model are
// not sent.
func codeIndexRerankTarget(cfg config.Config) (base, key, model string, err error) {
	explicit := strings.TrimSpace(cfg.CodeIndexRerankProvider)
	model = strings.TrimSpace(cfg.CodeIndexRerankModel)
	if explicit == "" {
		id, embedKey, err := codeIndexTarget(cfg)
		if err != nil {
			return "", "", "", err
		}
		if model == "" {
			return "", "", "", fmt.Errorf("code index rerank model is not set")
		}
		return id.BaseURL, embedKey, model, nil
	}
	base, key, provModel, ok := lookupEmbeddingProvider(cfg, explicit)
	if !ok {
		return "", "", "", fmt.Errorf("code index rerank provider %q is not configured", explicit)
	}
	if model == "" {
		model = strings.TrimSpace(provModel)
	}
	if model == "" {
		return "", "", "", fmt.Errorf("code index rerank model is not set")
	}
	return base, key, model, nil
}

// ReindexCode embeds every current file again. The feature has to be on,
// and an embedding model has to resolve, before anything is written.
func (a *Agent) ReindexCode(ctx context.Context) error {
	if !a.CodeIndexEnabled() {
		return fmt.Errorf("code index is off")
	}
	id, emb, err := a.prepareCodeIndex()
	if err != nil {
		return err
	}
	return a.ensureCodeIndex().RebuildFor(ctx, emb, id, a.WorkingDirSnapshot())
}
