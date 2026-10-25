package projectfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gogen/internal/config"
)

func TestMergeOverProjectWinsAndInherits(t *testing.T) {
	base := config.Defaults()
	base.OpenAIModel = "global-model"
	base.OpenAIURL = "http://global/v1"
	base.OpenAIProviders = []config.OpenAIProviderConfig{{
		Name: "global", BaseURL: "http://global/v1", Model: "global-model",
	}}

	inherited := MergeOver(&base, &ProjectFile{}, FlagOverrides{})
	if inherited.OpenAIModel != "global-model" || inherited.OpenAIURL != "http://global/v1" {
		t.Fatalf("empty project did not inherit global: model=%q url=%q", inherited.OpenAIModel, inherited.OpenAIURL)
	}
	if len(inherited.OpenAIProviders) != 1 || inherited.OpenAIProviders[0].Name != "global" {
		t.Fatalf("empty project did not inherit providers: %+v", inherited.OpenAIProviders)
	}

	project := &ProjectFile{Config: FileConfig{
		OpenAIModel: "project-model",
		OpenAIProviders: []OpenAIProviderEntry{{
			Name: "local", BaseURL: "http://local/v1", Model: "local-model",
		}},
	}}
	over := MergeOver(&base, project, FlagOverrides{})
	if over.OpenAIModel != "project-model" {
		t.Fatalf("project model = %q, want project-model", over.OpenAIModel)
	}
	if over.OpenAIURL != "http://global/v1" {
		t.Fatalf("unset project url = %q, want the global url", over.OpenAIURL)
	}
	if len(over.OpenAIProviders) != 1 || over.OpenAIProviders[0].Name != "local" {
		t.Fatalf("project providers = %+v, want the project list", over.OpenAIProviders)
	}
}

func TestSavePinnedProjectConfigLeavesInheritedKeysOut(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gogen.conf")
	seed := "openai_model: pinned-model\n"
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg := config.Defaults()
	cfg.OpenAIModel = "pinned-model"
	cfg.OpenAIURL = "http://global/v1"
	cfg.Board = "on"
	if err := SavePinnedProjectConfig(path, &cfg, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "pinned-model") {
		t.Fatalf("pinned model was dropped:\n%s", body)
	}
	if strings.Contains(body, "global/v1") || strings.Contains(body, "board:") {
		t.Fatalf("inherited settings were copied into the project file:\n%s", body)
	}
}

func TestSeedGlobalConfigSkipsEmpty(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := config.Defaults()
	if err := SeedGlobalConfig(&cfg, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(GlobalConfigPath()); !os.IsNotExist(err) {
		t.Fatalf("empty project created a global config: %v", err)
	}
}

func TestSeedGlobalConfigOnce(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", t.TempDir())
	cfg := config.Defaults()
	cfg.OpenAIModel = "shared-model"
	cfg.WorkingDir = t.TempDir()
	cfg.TestCommand = "make test"
	if err := SeedGlobalConfig(&cfg, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(GlobalConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	body := string(data)
	if !strings.Contains(body, "shared-model") {
		t.Fatalf("seed missing model:\n%s", body)
	}
	if strings.Contains(body, "make test") || strings.Contains(body, cfg.WorkingDir) {
		t.Fatalf("seed copied a project-only field:\n%s", body)
	}
	cfg.OpenAIModel = "later-model"
	if err := SeedGlobalConfig(&cfg, WriteOptions{}); err != nil {
		t.Fatal(err)
	}
	again, err := os.ReadFile(GlobalConfigPath())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(again), "later-model") {
		t.Fatal("seed overwrote an existing global config")
	}
}

func TestLoadAdoptConfReadsProjectFile(t *testing.T) {
	dir := t.TempDir()
	conf := filepath.Join(dir, ".gogen", "gogen.conf")
	if err := os.MkdirAll(filepath.Dir(conf), 0o755); err != nil {
		t.Fatal(err)
	}
	body := "openai_base_url: http://example/v1\nopenai_providers:\n  - name: local\n    base_url: http://example/v1\n"
	if err := os.WriteFile(conf, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv(AdoptConfEnv, conf)
	gotDir, pf, ok := LoadAdoptConf()
	if !ok || pf == nil {
		t.Fatal("adopt conf was not loaded")
	}
	if pf.Config.OpenAIBaseURL != "http://example/v1" {
		t.Fatalf("base url = %q", pf.Config.OpenAIBaseURL)
	}
	if len(pf.Config.OpenAIProviders) != 1 || pf.Config.OpenAIProviders[0].Name != "local" {
		t.Fatalf("providers = %+v", pf.Config.OpenAIProviders)
	}
	if filepath.Clean(gotDir) != filepath.Clean(dir) {
		t.Fatalf("project dir = %q, want %q", gotDir, dir)
	}
}

func TestLoadAdoptConfUnset(t *testing.T) {
	t.Setenv(AdoptConfEnv, "")
	if _, _, ok := LoadAdoptConf(); ok {
		t.Fatal("unset adopt env loaded a config")
	}
}
