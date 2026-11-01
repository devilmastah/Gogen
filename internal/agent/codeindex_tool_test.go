package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"gogen/internal/codeindex"
	"gogen/internal/config"
	"gogen/internal/llm"
)

func TestSearchMeaningOff(t *testing.T) {
	dir := t.TempDir()
	a := NewAgent(nil, NewExecutor(dir), nil)
	for _, ft := range a.featureTools() {
		if ft.Name == "search_meaning" {
			t.Fatal("search_meaning is registered while code index is off")
		}
	}
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("off tool err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(err) {
		t.Fatal("startup created .gogen/index")
	}
}

func TestSearchMeaningNoProvider(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("alpha\n"), 0o644)
	a := NewAgent(nil, NewExecutor(dir), nil)
	ix := codeindex.Open(dir)
	t.Cleanup(func() { ix.SetWatch(false, nil) })
	a.SetCodeIndex(ix)
	a.SetCodeIndexEnabled(true)
	emb := &recEmb{}
	a.SetCodeIndexEmbedder(emb)
	cfg := config.Defaults()
	cfg.OpenAIModel = "chat-model"
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	for _, ft := range a.featureTools() {
		if ft.Name == "search_meaning" {
			t.Fatal("search_meaning is registered without a provider")
		}
	}
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}}); err == nil || !strings.Contains(err.Error(), "unknown tool") {
		t.Fatalf("tool err = %v", err)
	}
	if err := a.ReindexCode(context.Background()); err == nil || !strings.Contains(err.Error(), "provider is not set") {
		t.Fatalf("reindex err = %v", err)
	}
	a.SyncCodeIndexWatch()
	if ix.Watching() {
		t.Fatal("watch started without a provider")
	}
	if len(emb.texts) != 0 {
		t.Fatalf("embedder called: %v", emb.texts)
	}
	if _, statErr := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(statErr) {
		t.Fatal("missing provider wrote .gogen/index")
	}
}

func TestSearchMeaningNamedProvider(t *testing.T) {
	var gotModel, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		var body struct {
			Model string   `json:"model"`
			Input []string `json:"input"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		gotModel = body.Model
		if strings.Contains(body.Model, "chat-model") {
			t.Errorf("chat model was sent: %s", body.Model)
		}
		vecs := make([]map[string]any, len(body.Input))
		for i := range body.Input {
			vecs[i] = map[string]any{"index": i, "embedding": []float64{1, 0}}
		}
		json.NewEncoder(w).Encode(map[string]any{"data": vecs})
	}))
	defer srv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("alpha\n"), 0o644)
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetCodeIndexEnabled(true)
	cfg := config.Defaults()
	cfg.CodeIndexProvider = "embed"
	cfg.OpenAIModel = "chat-model"
	cfg.OpenAIKey = "sk-chat"
	cfg.OpenAIURL = "http://chat.example/v1"
	cfg.OpenAIProviders = []config.OpenAIProviderConfig{{
		Name: "embed", BaseURL: srv.URL + "/v1", APIKey: "sk-embed", Model: "bge-small",
	}}
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	out, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "a.go") {
		t.Fatalf("result = %s", out)
	}
	if gotModel != "bge-small" {
		t.Fatalf("model = %q", gotModel)
	}
	if gotAuth != "Bearer sk-embed" {
		t.Fatalf("auth = %q", gotAuth)
	}
	raw, err := os.ReadFile(filepath.Join(dir, ".gogen", "index", "meta.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(raw), "sk-embed") || strings.Contains(string(raw), "sk-chat") {
		t.Fatalf("meta contains a key: %s", raw)
	}
	if !strings.Contains(string(raw), "bge-small") || !strings.Contains(string(raw), srv.URL) {
		t.Fatalf("meta = %s", raw)
	}
}

func TestSearchMeaningUnknownProvider(t *testing.T) {
	dir := t.TempDir()
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetCodeIndexEnabled(true)
	emb := &recEmb{}
	a.SetCodeIndexEmbedder(emb)
	cfg := config.Defaults()
	cfg.CodeIndexProvider = "missing"
	cfg.CodeIndexModel = "m"
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}}); err == nil {
		t.Fatal("expected an error")
	}
	if len(emb.texts) != 0 {
		t.Fatal("embedder was called")
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(err) {
		t.Fatal("unknown provider wrote .gogen/index")
	}
}

func TestSearchMeaningChatModelDoesNotRebuild(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("alpha\n"), 0o644)
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetCodeIndexEnabled(true)
	emb := &recEmb{vec: func(string) []float32 { return []float32{1, 0} }}
	a.SetCodeIndexEmbedder(emb)
	cfg := config.Defaults()
	cfg.CodeIndexProvider = "embed"
	cfg.OpenAIModel = "chat-a"
	cfg.OpenAIProviders = []config.OpenAIProviderConfig{{Name: "embed", BaseURL: "http://embed", Model: "bge"}}
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}}); err != nil {
		t.Fatal(err)
	}
	n := emb.count("alpha")
	cfg.OpenAIModel = "chat-b"
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}}); err != nil {
		t.Fatal(err)
	}
	if emb.count("alpha") != n {
		t.Fatal("changing the chat model re-embedded the file")
	}
}

func TestSearchMeaningPlanMode(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("alpha\n"), 0o644)
	before := relSet(t, dir)
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetCodeIndexEnabled(true)
	a.SetMode(ModePlan)
	a.SetCodeIndexEmbedder(&recEmb{vec: func(string) []float32 { return []float32{1, 0} }})
	cfg := config.Defaults()
	cfg.CodeIndexProvider = "default"
	cfg.CodeIndexModel = "m"
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	if _, ok := a.AllowedToolNames()["search_meaning"]; !ok {
		t.Fatal("plan mode does not allow search_meaning")
	}
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}}); err != nil {
		t.Fatal(err)
	}
	for p := range relSet(t, dir) {
		if before[p] || p == ".gogen" || p == ".gogen/index" || strings.HasPrefix(p, ".gogen/index/") {
			continue
		}
		t.Fatalf("plan mode created %s", p)
	}
}

func TestSearchMeaningDescription(t *testing.T) {
	def := searchMeaningToolDef()
	if def.Description != searchMeaningDescription {
		t.Fatalf("description = %q", def.Description)
	}
	if !strings.Contains(def.Description, "search_code") || !strings.Contains(def.Description, "path") {
		t.Fatal(def.Description)
	}
}

func TestSearchMeaningOffAgain(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("alpha\n"), 0o644)
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetCodeIndexEnabled(true)
	a.SetCodeIndexEmbedder(&recEmb{vec: func(string) []float32 { return []float32{1, 0} }})
	cfg := config.Defaults()
	cfg.CodeIndexProvider = "default"
	cfg.CodeIndexModel = "m"
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}}); err != nil {
		t.Fatal(err)
	}
	a.SetCodeIndexEnabled(false)
	for _, ft := range a.featureTools() {
		if ft.Name == "search_meaning" {
			t.Fatal("tool still registered")
		}
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "index", "meta.json")); err != nil {
		t.Fatal("turning the feature off removed the index")
	}
}

func TestReindexCode(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "a.go"), []byte("alpha\n"), 0o644)
	a := NewAgent(nil, NewExecutor(dir), nil)
	if err := a.ReindexCode(context.Background()); err == nil || !strings.Contains(err.Error(), "off") {
		t.Fatalf("off err = %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(err) {
		t.Fatal("reindex while off created .gogen/index")
	}
	a.SetCodeIndexEnabled(true)
	cfg := config.Defaults()
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	if err := a.ReindexCode(context.Background()); err == nil {
		t.Fatal("reindex with no model should fail")
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(err) {
		t.Fatal("reindex with no model created .gogen/index")
	}
	cfg.CodeIndexProvider = "default"
	cfg.CodeIndexModel = "m"
	emb := &recEmb{vec: func(string) []float32 { return []float32{1, 0} }}
	a.SetCodeIndexEmbedder(emb)
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.ReindexCode(context.Background()); err != nil {
		t.Fatal(err)
	}
	if emb.count("alpha") != 2 {
		t.Fatalf("reindex embed count = %d", emb.count("alpha"))
	}
	snap := a.CodeIndex().Snapshot()
	if snap.Phase != codeindex.PhaseIdle || snap.FilesTotal != 1 || snap.ChunksTotal != 1 {
		t.Fatalf("snapshot = %+v", snap)
	}

	emb.block = make(chan struct{})
	emb.release = make(chan struct{})
	done := make(chan error, 1)
	go func() { done <- a.ReindexCode(context.Background()) }()
	<-emb.block
	if err := a.ReindexCode(context.Background()); err != codeindex.ErrBusy {
		t.Fatalf("second reindex = %v", err)
	}
	close(emb.release)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}

func TestSearchMeaningRerankOff(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "alpha.go"), []byte("alpha\n"), 0o644)
	os.WriteFile(filepath.Join(dir, "beta.go"), []byte("beta\n"), 0o644)
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetCodeIndexEnabled(true)
	a.SetCodeIndexEmbedder(&recEmb{vec: func(text string) []float32 {
		if strings.Contains(text, "beta") {
			return []float32{0, 1}
		}
		return []float32{1, 0}
	}})
	rr := &recRank{}
	a.SetCodeIndexReranker(rr)
	cfg := config.Defaults()
	cfg.CodeIndexProvider = "default"
	cfg.CodeIndexModel = "m"
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	out, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}})
	if err != nil {
		t.Fatal(err)
	}
	if rr.calls != 0 {
		t.Fatal("rerank endpoint was called while rerank is off")
	}
	alpha := strings.Index(out, "alpha.go")
	beta := strings.Index(out, "beta.go")
	if alpha < 0 || beta < 0 || alpha > beta {
		t.Fatalf("cosine order lost:\n%s", out)
	}
}

func TestSearchMeaningRerankTarget(t *testing.T) {
	var mu sync.Mutex
	var gotModel, gotAuth, gotPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Model     string   `json:"model"`
			Documents []string `json:"documents"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		mu.Lock()
		gotModel = body.Model
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		mu.Unlock()
		results := make([]map[string]any, len(body.Documents))
		for i := range body.Documents {
			results[i] = map[string]any{"index": i, "relevance_score": 1}
		}
		json.NewEncoder(w).Encode(map[string]any{"results": results})
	}))
	defer srv.Close()

	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "alpha.go"), []byte("alpha\n"), 0o644)
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetCodeIndexEnabled(true)
	a.SetCodeIndexEmbedder(&recEmb{vec: func(string) []float32 { return []float32{1, 0} }})
	cfg := config.Defaults()
	cfg.CodeIndexProvider = "embed"
	cfg.CodeIndexModel = "bge"
	cfg.CodeIndexRerank = "on"
	cfg.CodeIndexRerankProvider = "rerank"
	cfg.OpenAIProviders = []config.OpenAIProviderConfig{
		{Name: "embed", BaseURL: srv.URL + "/v1", APIKey: "sk-embed", Model: "bge"},
		{Name: "rerank", BaseURL: srv.URL + "/v1", APIKey: "sk-rerank", Model: "rerank-own"},
	}
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	model, auth, path := gotModel, gotAuth, gotPath
	mu.Unlock()
	if path != "/v1/rerank" || model != "rerank-own" || auth != "Bearer sk-rerank" {
		t.Fatalf("rerank call path=%q model=%q auth=%q", path, model, auth)
	}

	cfg.CodeIndexRerankProvider = ""
	cfg.CodeIndexRerankModel = "qwen3-reranker"
	if _, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}}); err != nil {
		t.Fatal(err)
	}
	mu.Lock()
	model, auth = gotModel, gotAuth
	mu.Unlock()
	if model != "qwen3-reranker" || auth != "Bearer sk-embed" {
		t.Fatalf("embedding-provider rerank model=%q auth=%q", model, auth)
	}
}

func TestSearchMeaningRerankModelMissing(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "alpha.go"), []byte("alpha\n"), 0o644)
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetCodeIndexEnabled(true)
	a.SetCodeIndexEmbedder(&recEmb{vec: func(string) []float32 { return []float32{1, 0} }})
	cfg := config.Defaults()
	cfg.CodeIndexProvider = "default"
	cfg.CodeIndexModel = "embed-model"
	cfg.CodeIndexRerank = "on"
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	out, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Rerank failed:") || !strings.Contains(out, "code index rerank model is not set") || !strings.Contains(out, "alpha.go") {
		t.Fatalf("result = %q", out)
	}

	cfg.CodeIndexRerankProvider = "missing"
	cfg.CodeIndexRerankModel = "qwen3-reranker"
	out, err = a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "Rerank failed:") || !strings.Contains(out, "not configured") || !strings.Contains(out, "alpha.go") {
		t.Fatalf("result = %q", out)
	}
}

type recRank struct {
	calls int
}

func (r *recRank) Rerank(context.Context, string, []string) ([]codeindex.RerankHit, error) {
	r.calls++
	return nil, context.Canceled
}

type recEmb struct {
	mu      sync.Mutex
	texts   []string
	vec     func(string) []float32
	block   chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *recEmb) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	r.mu.Lock()
	r.texts = append(r.texts, texts...)
	r.mu.Unlock()
	if r.block != nil {
		r.once.Do(func() { close(r.block) })
		<-r.release
	}
	out := make([][]float32, len(texts))
	for i, text := range texts {
		if r.vec != nil {
			out[i] = r.vec(text)
		} else {
			out[i] = []float32{1, 0}
		}
	}
	return out, nil
}

func (r *recEmb) count(s string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, t := range r.texts {
		if strings.Contains(t, s) {
			n++
		}
	}
	return n
}

func TestCodeIndexWatchFeatureOff(t *testing.T) {
	dir := t.TempDir()
	a := NewAgent(nil, NewExecutor(dir), nil)
	ix := codeindex.Open(dir)
	t.Cleanup(func() { ix.SetWatch(false, nil) })
	a.SetCodeIndex(ix)
	cfg := config.Defaults()
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	a.SyncCodeIndexWatch()
	if ix.Watching() {
		t.Fatal("code index off started a watch")
	}
	if err := os.WriteFile(filepath.Join(dir, "a.go"), []byte("alpha\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, ".gogen", "index")); !os.IsNotExist(err) {
		t.Fatal("code index off created .gogen/index")
	}

	a.SetCodeIndexEnabled(true)
	a.SyncCodeIndexWatch()
	if ix.Watching() {
		t.Fatal("no provider started a watch")
	}
	cfg.CodeIndexProvider = "default"
	a.SyncCodeIndexWatch()
	waitUntil(t, ix.Watching)
	cfg.CodeIndexWatch = "off"
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	a.SyncCodeIndexWatch()
	if ix.Watching() {
		t.Fatal("explicit off left the watch running")
	}
}

func TestReindexCodeOtherFolder(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "secret.go"), []byte("TOPSECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetCodeIndexEnabled(true)
	ix := codeindex.Open(dir)
	a.SetCodeIndex(ix)
	ix.SetWorkingDir(other)
	emb := &recEmb{}
	a.SetCodeIndexEmbedder(emb)
	cfg := config.Defaults()
	cfg.CodeIndexProvider = "default"
	cfg.CodeIndexModel = "embed-model"
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	err := a.ReindexCode(context.Background())
	if err == nil || !strings.Contains(err.Error(), "different folder") {
		t.Fatalf("err = %v", err)
	}
	if len(emb.texts) != 0 {
		t.Fatal("embedder was called")
	}
	if _, err := os.Stat(filepath.Join(other, ".gogen", "index")); !os.IsNotExist(err) {
		t.Fatalf("other folder index: %v", err)
	}
}

func TestSearchMeaningOtherFolder(t *testing.T) {
	dir := t.TempDir()
	other := t.TempDir()
	if err := os.WriteFile(filepath.Join(other, "secret.go"), []byte("TOPSECRET\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	a := NewAgent(nil, NewExecutor(dir), nil)
	a.SetCodeIndexEnabled(true)
	ix := codeindex.Open(dir)
	a.SetCodeIndex(ix)
	ix.SetWorkingDir(other)
	emb := &recEmb{}
	a.SetCodeIndexEmbedder(emb)
	cfg := config.Defaults()
	cfg.CodeIndexProvider = "default"
	cfg.CodeIndexModel = "embed-model"
	a.SetCodeIndexConfig(func() config.Config { return cfg })
	out, err := a.executeTool(context.Background(), llm.ToolCall{Name: "search_meaning", Args: map[string]any{"query": "q"}})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out, "different folder") || strings.Contains(out, "TOPSECRET") {
		t.Fatalf("result = %q", out)
	}
	if len(emb.texts) != 0 {
		t.Fatal("embedder was called")
	}
}

func waitUntil(t *testing.T, ready func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if ready() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func relSet(t *testing.T, dir string) map[string]bool {
	t.Helper()
	out := map[string]bool{}
	filepath.WalkDir(dir, func(path string, d os.DirEntry, err error) error {
		if err != nil || path == dir {
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return nil
		}
		out[filepath.ToSlash(rel)] = true
		return nil
	})
	return out
}
