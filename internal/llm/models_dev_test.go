package llm

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"gogen/internal/modelinfo"
)

func TestLookupModelsDevLimitFromDisk(t *testing.T) {
	reg := map[string]any{
		"opencode": map[string]any{
			"id":  "opencode",
			"api": "https://opencode.ai/zen/v1",
			"models": map[string]any{
				"claude-opus-4-8": map[string]any{
					"id":          "claude-opus-4-8",
					"description": "Claude Opus 4.8",
					"limit":       map[string]int{"context": 1000000, "output": 128000},
				},
			},
		},
		"opencode-go": map[string]any{
			"id":  "opencode-go",
			"api": "https://opencode.ai/zen/go/v1",
			"models": map[string]any{
				"mimo-v2.5-pro": map[string]any{
					"id":    "mimo-v2.5-pro",
					"limit": map[string]int{"context": 1048576, "output": 128000},
				},
			},
		},
	}
	dir := t.TempDir()
	cache := filepath.Join(dir, "models.json")
	b, err := json.Marshal(reg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, b, 0o644); err != nil {
		t.Fatal(err)
	}

	p := &OpenAIProvider{
		profiles:  []*providerProfile{{name: "default", baseURL: "https://opencode.ai/zen/v1/"}},
		modelInfo: modelinfo.NewResolver(cache),
	}

	start := time.Now()
	got, _, _, desc := p.lookupModelsDevInfo(ModelRef{ID: "claude-opus-4-8"})
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("lookup blocked for %v (disk cache should be instant)", elapsed)
	}
	if got != 1000000 {
		t.Fatalf("zen model: got %d, want 1000000", got)
	}
	if desc != "Claude Opus 4.8" {
		t.Fatalf("description = %q, want %q", desc, "Claude Opus 4.8")
	}

	got, _, _, _ = p.lookupModelsDevInfo(ModelRef{ID: "mimo-v2.5-pro"})
	if got != 1048576 {
		t.Fatalf("go model via dual URL: got %d, want 1048576", got)
	}
}

func TestLookupModelsDevInfoNilSafe(t *testing.T) {
	var p *OpenAIProvider
	if got, _, _, _ := p.lookupModelsDevInfo(ModelRef{ID: "x"}); got != 0 {
		t.Fatalf("got %d", got)
	}
	p = &OpenAIProvider{}
	if got, _, _, _ := p.lookupModelsDevInfo(ModelRef{ID: "x"}); got != 0 {
		t.Fatalf("got %d", got)
	}
}
