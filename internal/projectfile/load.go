package projectfile

import (
	"log"
	"os"
	"strings"
)

// LoadGlobalConfig loads the global config file (~/.config/gogen/config.yaml).
// Returns nil if the file does not exist or cannot be parsed (non-fatal).
func LoadGlobalConfig() *ProjectFile {
	path := GlobalConfigPath()
	data, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	// Global config is pure YAML (same format as .gogen/gogen.conf).
	cfg, err := parseYAMLConfig(string(data))
	if err != nil {
		return nil
	}
	pf := &ProjectFile{Path: path, HasConfig: true, Config: cfg}

	// Load global guidelines from ~/.config/gogen/gogen.md (optional).
	gPath := GlobalGuidelinesPath()
	if gData, gErr := os.ReadFile(gPath); gErr == nil {
		body := strings.TrimSpace(string(gData))
		if body != "" {
			pf.Guidelines = body
		}
	}
	return pf
}

// ConfigFileHasSecrets reports whether the config file at path stores secret
// material — the fields buildConfigYAML only writes when IncludeSecrets is
// set (openai_api_key, MCP server env). Handles .conf files, the plain-YAML
// global config (config.yaml), and front-matter .md files. Used by the live
// feature-toggle persist so a rewrite never silently drops a stored key.
//
// An unparseable file that still exists is conservatively treated as having
// secrets: dropping a stored key is data loss, whereas writing a key that
// only came from the environment is a minor surprise.
func ConfigFileHasSecrets(path string) bool {
	data, err := os.ReadFile(path)
	if err != nil {
		return false // no file yet — nothing stored to preserve
	}
	cfg, err := parseYAMLConfig(string(data))
	if err != nil {
		// Not pure YAML (front-matter .md, or corrupt): fall back to the
		// front-matter-aware loader before assuming the worst.
		pf, loadErr := Load(path)
		if loadErr != nil || pf == nil {
			log.Printf("config: %s unreadable, assuming it stores secrets", path)
			return true
		}
		cfg = pf.Config
	}
	if cfg.OpenAIAPIKey != "" {
		return true
	}
	if cfg.WebAuthToken != "" {
		return true
	}
	if cfg.StatusDisplayToken != "" {
		return true
	}
	for _, p := range cfg.OpenAIProviders {
		if p.APIKey != "" {
			return true
		}
	}
	for _, m := range cfg.MCPServers {
		if len(m.Env) > 0 {
			return true
		}
	}
	return false
}
