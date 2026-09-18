package llm

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"
)

func TestListModelsCachesSuccessfulFetch(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"object": "list",
			"data": []map[string]any{
				{"id": "model-a", "object": "model"},
			},
		})
	}))
	defer srv.Close()

	client := openai.NewClient(
		option.WithBaseURL(srv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(srv.Client()),
	)
	p := &OpenAIProvider{
		profiles:    []*providerProfile{{name: "default", stream: &client}},
		modelClient: make(map[ModelRef]*openai.Client),
	}

	ctx := context.Background()
	first, err := p.listModels(ctx)
	if err != nil {
		t.Fatalf("first listModels: %v", err)
	}
	if len(first) != 1 || first[0].model.ID != "model-a" {
		t.Fatalf("unexpected first result: %+v", first)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected 1 hit after first call, got %d", got)
	}

	second, err := p.listModels(ctx)
	if err != nil {
		t.Fatalf("second listModels: %v", err)
	}
	if len(second) != 1 || second[0].model.ID != "model-a" {
		t.Fatalf("unexpected second result: %+v", second)
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("expected cache hit (still 1 request), got %d", got)
	}
}

func TestFetchModelsQueriesOpenCodeEndpointsInParallel(t *testing.T) {
	started := make(chan struct{}, 2)
	release := make(chan struct{})

	handler := func(id string) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/models" {
				http.NotFound(w, r)
				return
			}
			started <- struct{}{}
			<-release
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"object": "list",
				"data": []map[string]any{
					{"id": id, "object": "model"},
				},
			})
		}
	}

	zenSrv := httptest.NewServer(handler("zen-model"))
	defer zenSrv.Close()
	goSrv := httptest.NewServer(handler("go-model"))
	defer goSrv.Close()

	zenClient := openai.NewClient(
		option.WithBaseURL(zenSrv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(zenSrv.Client()),
	)
	goClient := openai.NewClient(
		option.WithBaseURL(goSrv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(goSrv.Client()),
	)
	p := &OpenAIProvider{
		profiles: []*providerProfile{{
			name:      "default",
			zenStream: &zenClient,
			goStream:  &goClient,
		}},
		modelClient: make(map[ModelRef]*openai.Client),
	}

	errCh := make(chan error, 1)
	var models []catalogModel
	go func() {
		var err error
		models, _, err = p.fetchModelsWithProfiles(context.Background(), true)
		errCh <- err
	}()

	deadline := time.After(2 * time.Second)
	for i := 0; i < 2; i++ {
		select {
		case <-started:
		case <-deadline:
			t.Fatal("endpoints were not queried concurrently")
		}
	}
	close(release)

	if err := <-errCh; err != nil {
		t.Fatalf("fetchModels: %v", err)
	}
	if len(models) != 2 {
		t.Fatalf("expected 2 models, got %d (%v)", len(models), models)
	}
}

func TestListModelsHonorsCallerDeadline(t *testing.T) {
	started := make(chan struct{}, 1)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		select {
		case started <- struct{}{}:
		default:
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := openai.NewClient(
		option.WithBaseURL(srv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(srv.Client()),
	)
	p := &OpenAIProvider{
		profiles:    []*providerProfile{{name: "default", stream: &client}},
		modelClient: make(map[ModelRef]*openai.Client),
	}

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	_, err := p.listModels(ctx)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected deadline error from hung /models")
	}
	if elapsed > 2*time.Second {
		t.Fatalf("listModels ignored caller deadline: took %s", elapsed)
	}
	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("server never saw /models request")
	}
}

// TestCatalogProbeFailureDoesNotArmBackoff verifies the brief catalog lookup
// used by ModelContextLimit (listModelsProbe) does not arm the shared
// modelsFetchFailedAt backoff when its short budget cuts the fetch off, while
// a full catalog fetch (listModels) still does. A slow-but-alive remote
// catalog that misses the 1.5s probe budget must not make clientForModel skip
// discovery for modelsFetchBackoff and route every model through the
// fallback/owner inference.
func TestCatalogProbeFailureDoesNotArmBackoff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		// Hang until the caller's budget aborts the request.
		<-r.Context().Done()
	}))
	defer srv.Close()

	client := openai.NewClient(
		option.WithBaseURL(srv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(srv.Client()),
	)
	p := &OpenAIProvider{
		profiles:    []*providerProfile{{name: "default", stream: &client}},
		modelClient: make(map[ModelRef]*openai.Client),
	}

	probeCtx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := p.listModelsProbe(probeCtx); err == nil {
		t.Fatal("expected probe fetch to fail on a hung /models")
	}
	if p.catalogFetchOnBackoff() {
		t.Fatal("brief probe failure must not arm the shared catalog backoff")
	}

	fullCtx, fullCancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer fullCancel()
	if _, err := p.listModels(fullCtx); err == nil {
		t.Fatal("expected full fetch to fail on a hung /models")
	}
	if !p.catalogFetchOnBackoff() {
		t.Fatal("full-budget catalog fetch failure must arm the shared backoff")
	}
}

// TestModelContextLimitProbeDoesNotArmBackoff verifies the ModelContextLimit
// probe path leaves modelsFetchFailedAt unset when its /v1/models lookup
// fails, so a following clientForModel discovery attempt is not suppressed by
// the shared backoff.
func TestModelContextLimitProbeDoesNotArmBackoff(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" {
			http.NotFound(w, r)
			return
		}
		hits.Add(1)
		// Fail fast: openai-go retries 5xx with exponential backoff, which
		// would stall the test for no coverage benefit.
		w.Header().Set("x-should-retry", "false")
		http.Error(w, "down", http.StatusInternalServerError)
	}))
	defer srv.Close()

	client := openai.NewClient(
		option.WithBaseURL(srv.URL+"/"),
		option.WithAPIKey("test"),
		option.WithHTTPClient(srv.Client()),
	)
	p := &OpenAIProvider{
		profiles:    []*providerProfile{{name: "default", stream: &client}},
		modelClient: make(map[ModelRef]*openai.Client),
		model:       "unknown-model",
	}

	if _, err := p.ModelContextLimit(context.Background()); err != nil {
		t.Fatalf("ModelContextLimit: %v", err)
	}
	if n := hits.Load(); n == 0 {
		t.Fatal("ModelContextLimit did not probe /v1/models")
	}
	if p.catalogFetchOnBackoff() {
		t.Fatal("ModelContextLimit's brief probe must not arm the shared catalog backoff")
	}
}
