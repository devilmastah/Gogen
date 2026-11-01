package server

import (
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"gogen/internal/config"
	"gogen/internal/llm"
	llmtest "gogen/internal/llm/llmtest"
	"gogen/internal/projectfile"

	"github.com/gorilla/websocket"
)

// drainHandshake consumes the attach-handshake frames (session_state +
// basic + full config) so later reads are unambiguous.
func drainHandshake(t *testing.T, conn *websocket.Conn) {
	t.Helper()
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "session_state" })
	// The handshake sends basic + full config, both carrying the decorated
	// provider list; wait until both are gone.
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" && len(m.Providers) > 0 })
	readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "config" })
}

// TestProviderSaveViaWS drives provider_save through a real WebSocket: the
// pushed config carries the new profile (apiKeySet, never the key itself),
// the default entry is present and not deletable, the key is persisted to
// the config file, and a blank key on a later save keeps the stored key.
func TestProviderSaveViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	send := func(op ProviderOpRequest) {
		t.Helper()
		if err := conn.WriteJSON(WSMessage{Type: "provider_save", ProviderOp: &op}); err != nil {
			t.Fatalf("send provider_save: %v", err)
		}
	}
	send(ProviderOpRequest{Name: "local-llama", BaseURL: "http://127.0.0.1:8080/v1", APIKey: "llama-key", Model: "llama3.1"})

	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		if len(m.Providers) == 0 {
			return false
		}
		for _, p := range m.Providers {
			if p.Name == "local-llama" {
				return true
			}
		}
		return false
	})
	if cfg.Providers[0].Name != "default" || cfg.Providers[0].Deletable {
		t.Fatalf("default entry must be first and not deletable: %+v", cfg.Providers[0])
	}
	var found *ProviderEntry
	for i := range cfg.Providers {
		if cfg.Providers[i].Name == "local-llama" {
			found = &cfg.Providers[i]
		}
	}
	if found == nil {
		t.Fatalf("local-llama missing from pushed list: %+v", cfg.Providers)
	}
	if found.BaseURL != "http://127.0.0.1:8080/v1" || !found.APIKeySet || !found.Deletable || found.Model != "llama3.1" {
		t.Fatalf("pushed provider = %+v, want baseURL + apiKeySet + deletable + model", *found)
	}
	if cfg.ConfigFilePath == "" {
		t.Fatal("configFilePath must be pushed (storage warning)")
	}

	// The key is persisted (broadcast happens after the write in the
	// applyProviderList goroutine).
	path := projectfile.DefaultSavePath(dir)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "llama-key") {
		t.Fatalf("provider key not persisted:\n%s", data)
	}

	// A blank apiKey on an existing profile keeps the stored key.
	send(ProviderOpRequest{Name: "local-llama", BaseURL: "http://127.0.0.1:8080/v1"})
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return len(m.Providers) > 0 })
	got := s.ws.GetOpenAIProviders()
	if len(got) != 1 || got[0].Name != "local-llama" || got[0].APIKey != "llama-key" {
		t.Fatalf("blank key overwrote the stored key: %+v", got)
	}
}

// TestProviderDeleteViaWS removes a profile (models disappear from the
// pushed list) and refuses to delete the implicit default profile.
func TestProviderDeleteViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	s.ws.SetOpenAIProviders([]config.OpenAIProviderConfig{
		{Name: "local-llama", BaseURL: "http://127.0.0.1:8080/v1", APIKey: "k"},
		{Name: "other", BaseURL: "http://127.0.0.1:9999/v1"},
	})
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	if err := conn.WriteJSON(WSMessage{Type: "provider_delete", ProviderOp: &ProviderOpRequest{Name: "local-llama"}}); err != nil {
		t.Fatalf("send provider_delete: %v", err)
	}
	cfg := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool {
		if len(m.Providers) == 0 {
			return false
		}
		for _, p := range m.Providers {
			if p.Name == "local-llama" {
				return false
			}
		}
		return true
	})
	if len(cfg.Providers) != 2 { // default + other
		t.Fatalf("provider list after delete = %+v, want default + other", cfg.Providers)
	}

	// Deleting the default profile is refused.
	if err := conn.WriteJSON(WSMessage{Type: "provider_delete", ProviderOp: &ProviderOpRequest{Name: "default"}}); err != nil {
		t.Fatalf("send provider_delete default: %v", err)
	}
	resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "provider" || resp.Success {
		t.Fatalf("default-delete notice = %+v, want provider error", resp)
	}
	if got := s.ws.GetOpenAIProviders(); len(got) != 1 || got[0].Name != "other" {
		t.Fatalf("default delete mutated the list: %+v", got)
	}

	// Deleting an unregistered provider is refused.
	if err := conn.WriteJSON(WSMessage{Type: "provider_delete", ProviderOp: &ProviderOpRequest{Name: "nope"}}); err != nil {
		t.Fatalf("send provider_delete nope: %v", err)
	}
	resp = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
	if resp.Kind != "provider" || resp.Success {
		t.Fatalf("unregistered-delete notice = %+v, want provider error", resp)
	}
}

// TestInvalidProviderOpsRejected pins validation: a bad base URL and a
// missing name are rejected with provider notices, and the list is
// untouched.
func TestInvalidProviderOpsRejected(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	bad := []ProviderOpRequest{
		{Name: "x", BaseURL: "not-a-url"},
		{Name: "x", BaseURL: "ftp://host/v1"},
		{Name: "  "},
	}
	for _, op := range bad {
		if err := conn.WriteJSON(WSMessage{Type: "provider_save", ProviderOp: &op}); err != nil {
			t.Fatalf("send provider_save: %v", err)
		}
		resp := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "notice" })
		if resp.Kind != "provider" || resp.Success {
			t.Fatalf("invalid op %+v: notice = %+v, want provider error", op, resp)
		}
	}
	if got := s.ws.GetOpenAIProviders(); len(got) != 0 {
		t.Fatalf("invalid ops mutated the list: %+v", got)
	}
}

// TestTestProviderViaWS runs test_provider with an injected mock builder:
// the reply carries the model catalog on success and the error on failure;
// nothing is registered or persisted.
func TestTestProviderViaWS(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	s.providerTestBuilder = func(op ProviderOpRequest, _ string) (llm.LLMProvider, error) {
		if op.BaseURL == "http://broken" {
			return nil, fmt.Errorf("connection refused")
		}
		m := llmtest.NewMockProvider()
		m.Models = []llm.ModelInfo{{ID: "mock-model", ContextLimit: 128000}}
		return m, nil
	}
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	if err := conn.WriteJSON(WSMessage{Type: "test_provider", ProviderOp: &ProviderOpRequest{
		BaseURL: "http://127.0.0.1:8080/v1", APIKey: "k", Model: "mock-model",
	}}); err != nil {
		t.Fatalf("send test_provider: %v", err)
	}
	res := readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "provider_test" })
	if res.ProviderTest == nil || !res.ProviderTest.OK {
		t.Fatalf("provider_test reply = %+v, want ok", res.ProviderTest)
	}
	if len(res.ProviderTest.Models) != 1 || res.ProviderTest.Models[0].ID != "mock-model" {
		t.Fatalf("test models = %+v, want [mock-model]", res.ProviderTest.Models)
	}
	if res.ProviderTest.LatencyMs < 0 {
		t.Fatalf("negative latency: %d", res.ProviderTest.LatencyMs)
	}

	// Error path surfaces the builder error in the reply.
	if err := conn.WriteJSON(WSMessage{Type: "test_provider", ProviderOp: &ProviderOpRequest{BaseURL: "http://broken"}}); err != nil {
		t.Fatalf("send test_provider broken: %v", err)
	}
	res = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "provider_test" })
	if res.ProviderTest == nil || res.ProviderTest.OK || res.ProviderTest.Error == "" {
		t.Fatalf("broken provider_test reply = %+v, want error", res.ProviderTest)
	}

	// Nothing was registered by the tests.
	if got := s.ws.GetOpenAIProviders(); len(got) != 0 {
		t.Fatalf("test_provider registered providers: %+v", got)
	}
}

// TestTestProviderByNameUsesStoredCredentials verifies that testing a
// registered profile by name resolves its STORED base URL + key server-side
// (the client only knows apiKeySet and never holds the key).
func TestTestProviderByNameUsesStoredCredentials(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	s.ws.SetOpenAIProviders([]config.OpenAIProviderConfig{
		{Name: "local-llama", BaseURL: "http://127.0.0.1:8080/v1", APIKey: "stored-key", Model: "llama3.1"},
	})
	var gotOp ProviderOpRequest
	s.providerTestBuilder = func(op ProviderOpRequest, _ string) (llm.LLMProvider, error) {
		gotOp = op
		return llmtest.NewMockProvider(), nil
	}
	srv := startWSServer(t, s)
	defer srv.Close()

	conn := dialWS(t, srv, "/ws")
	defer conn.Close()
	drainHandshake(t, conn)

	// Name only — the client does not send the key.
	if err := conn.WriteJSON(WSMessage{Type: "test_provider", ProviderOp: &ProviderOpRequest{Name: "local-llama"}}); err != nil {
		t.Fatalf("send test_provider: %v", err)
	}
	_ = readUntil(t, conn, 5*time.Second, func(m WSMessage) bool { return m.Type == "provider_test" })
	if gotOp.BaseURL != "http://127.0.0.1:8080/v1" || gotOp.APIKey != "stored-key" || gotOp.Model != "llama3.1" {
		t.Fatalf("by-name test resolved to %+v, want stored credentials", gotOp)
	}
}

// TestEffectiveConfigOverlaysLiveValues pins the persistence snapshot: the
// startup config with every live-mutable value overlaid, so a later
// single-field persist never reverts an earlier live change.
func TestEffectiveConfigOverlaysLiveValues(t *testing.T) {
	dir := t.TempDir()
	stub := newBlockingStub()
	s, _, _ := newContinuationServer(t, stub, dir)
	if s.effectiveConfig() == nil {
		t.Fatal("effectiveConfig nil with a startup config")
	}
	s.ws.SetBoardEnabled(true)
	s.ws.SetSubagentEnabled(true)
	s.ws.SetSubagentMaxDepth(4)
	s.ws.SetOpenAIProviders([]config.OpenAIProviderConfig{{Name: "x", BaseURL: "http://x/v1", APIKey: "xk"}})
	eff := s.effectiveConfig()
	if eff.Board != "on" || eff.Subagent != "on" || eff.SubagentMaxDepth != 4 {
		t.Fatalf("live flags not overlaid: board=%q subagent=%q depth=%d", eff.Board, eff.Subagent, eff.SubagentMaxDepth)
	}
	if len(eff.OpenAIProviders) != 1 || eff.OpenAIProviders[0].Name != "x" || eff.OpenAIProviders[0].APIKey != "xk" {
		t.Fatalf("live providers not overlaid: %+v", eff.OpenAIProviders)
	}
}

func TestCodeIndexConfigUsesLiveProviders(t *testing.T) {
	ws := &Workspace{}
	ws.SetRuntimeConfig(config.Config{
		OpenAIURL:         "http://chat/v1",
		OpenAIKey:         "chat-key",
		CodeIndexProvider: "embed",
		OpenAIProviders: []config.OpenAIProviderConfig{{
			Name: "stale", BaseURL: "http://old/v1", APIKey: "old",
		}},
	})
	ws.SetOpenAIProviders([]config.OpenAIProviderConfig{{
		Name: "embed", BaseURL: "http://embed/v1", APIKey: "sk", Model: "bge",
	}})
	got := ws.codeIndexConfig()
	if got.OpenAIURL != "http://chat/v1" || got.OpenAIKey != "chat-key" || got.CodeIndexProvider != "embed" {
		t.Fatalf("overlay = url %q key %q provider %q", got.OpenAIURL, got.OpenAIKey, got.CodeIndexProvider)
	}
	if len(got.OpenAIProviders) != 1 || got.OpenAIProviders[0].Name != "embed" || got.OpenAIProviders[0].BaseURL != "http://embed/v1" || got.OpenAIProviders[0].APIKey != "sk" || got.OpenAIProviders[0].Model != "bge" {
		t.Fatalf("providers = %+v", got.OpenAIProviders)
	}
	got.OpenAIProviders[0].APIKey = "changed"
	live := ws.GetOpenAIProviders()
	if len(live) != 1 || live[0].APIKey != "sk" {
		t.Fatalf("live providers = %+v", live)
	}
}

func TestValidHTTPURL(t *testing.T) {
	cases := []struct {
		in   string
		want bool
	}{
		{"http://127.0.0.1:8080/v1", true},
		{"https://api.openai.com/v1", true},
		{"", false},
		{"not-a-url", false},
		{"ftp://host/v1", false},
		{"http://", false},
		{"javascript:alert(1)", false},
	}
	for _, tc := range cases {
		if got := validHTTPURL(tc.in); got != tc.want {
			t.Errorf("validHTTPURL(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

// TestProviderListFromConfigRoundTrips ensures the workspace seeds its live
// provider list from the startup config.
func TestProviderListFromConfig(t *testing.T) {
	cfg := &config.Config{OpenAIProviders: []config.OpenAIProviderConfig{{Name: "a", BaseURL: "http://a/v1", APIKey: "ak"}}}
	got := providerListFromConfig(cfg)
	if len(got) != 1 || got[0].Name != "a" || got[0].APIKey != "ak" {
		t.Fatalf("providerListFromConfig = %+v", got)
	}
	// The copy must not share the backing array.
	cfg.OpenAIProviders[0].Name = "mutated"
	if got[0].Name != "a" {
		t.Fatalf("providerListFromConfig shares the source array")
	}
	if providerListFromConfig(nil) != nil {
		t.Fatal("nil config should yield nil list")
	}
}
