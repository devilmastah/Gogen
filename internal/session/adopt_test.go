package session

import (
	"os"
	"path/filepath"
	"testing"

	"gogen/internal/config"
)

func TestLatestSavedModelSkipsNestedAndEmpty(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, ".gogen", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	index := `{
  "entries": [
    {"id": "old", "updated": "2026-10-25T10:00:00Z", "messageCount": 2},
    {"id": "nested", "updated": "2026-10-25T12:00:00Z", "messageCount": 4, "parentID": "old"},
    {"id": "blank", "updated": "2026-10-25T13:00:00Z", "messageCount": 0},
    {"id": "once", "updated": "2026-10-25T14:00:00Z", "messageCount": 3, "oneshot": true},
    {"id": "new", "updated": "2026-10-25T11:00:00Z", "messageCount": 8}
  ]
}`
	if err := os.WriteFile(filepath.Join(sessions, "index.json"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSession(t, sessions, "old", `{"model":"old-model","modelProvider":"other"}`)
	writeSession(t, sessions, "nested", `{"model":"nested-model","modelProvider":"other"}`)
	writeSession(t, sessions, "new", `{"model":"saved-model","modelProvider":"local"}`)

	model, provider := LatestSavedModel(dir)
	if model != "saved-model" || provider != "local" {
		t.Fatalf("latest model = %q provider %q, want saved-model / local", model, provider)
	}
}

func TestApplyLatestSavedModelUsesProvider(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, ".gogen", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	index := `{"entries":[{"id":"s1","updated":"2026-10-25T11:00:00Z","messageCount":2}]}`
	if err := os.WriteFile(filepath.Join(sessions, "index.json"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSession(t, sessions, "s1", `{"model":"saved-model","modelProvider":"local"}`)

	cfg := config.Defaults()
	cfg.OpenAIProviders = []config.OpenAIProviderConfig{{
		Name: "local", BaseURL: "http://example/v1",
	}}
	if !ApplyLatestSavedModel(&cfg, dir) {
		t.Fatal("expected the session model to be applied")
	}
	if cfg.OpenAIModel != "" {
		t.Fatalf("named provider model was stored as the default model: %q", cfg.OpenAIModel)
	}
	if cfg.OpenAIProviders[0].Model != "saved-model" {
		t.Fatalf("provider model = %q", cfg.OpenAIProviders[0].Model)
	}
	if ApplyLatestSavedModel(&cfg, dir) {
		t.Fatal("a second apply overwrote a model that was already set")
	}
}

func TestApplyLatestSavedModelDefaultProfile(t *testing.T) {
	dir := t.TempDir()
	sessions := filepath.Join(dir, ".gogen", "sessions")
	if err := os.MkdirAll(sessions, 0o755); err != nil {
		t.Fatal(err)
	}
	index := `{"entries":[{"id":"s1","updated":"2026-10-25T11:00:00Z","messageCount":1}]}`
	if err := os.WriteFile(filepath.Join(sessions, "index.json"), []byte(index), 0o644); err != nil {
		t.Fatal(err)
	}
	writeSession(t, sessions, "s1", `{"model":"default-model"}`)

	cfg := config.Defaults()
	cfg.OpenAIURL = "http://example/v1"
	if !ApplyLatestSavedModel(&cfg, dir) || cfg.OpenAIModel != "default-model" {
		t.Fatalf("default model = %q", cfg.OpenAIModel)
	}
}

func writeSession(t *testing.T, dir, id, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, id+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
