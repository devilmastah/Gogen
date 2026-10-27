package projectfile

import (
	"encoding/json"
	"log"
	"os"
	"strconv"
	"strings"

	"gogen/internal/config"
	"gogen/internal/onoff"
)

// Merge builds the effective runtime config: env > flags > file > defaults.
// CLI flag overrides (FlagOverrides) are applied only when the corresponding
// environment variable is not set, so env vars cannot be overridden by flags.
func Merge(pf *ProjectFile, flags FlagOverrides) *config.Config {
	def := config.Defaults()
	var file FileConfig
	if pf != nil {
		file = pf.Config
	}

	cfg := &config.Config{}
	// One loop over the settings table (settings.go) instead of one literal
	// line per setting: the table is the single declaration of the env var,
	// the file key and the default for every setting, so a setting cannot be
	// merged here and forgotten by the writer below.
	applySettings(cfg, &file, &def)

	if flags.WorkingDir != "" {
		if _, ok := os.LookupEnv("GOGEN_WORKING_DIR"); !ok {
			cfg.WorkingDir = flags.WorkingDir
		}
	}
	if flags.OpenAIURL != "" {
		if _, ok := os.LookupEnv("OPENAI_BASE_URL"); !ok {
			cfg.OpenAIURL = flags.OpenAIURL
		}
	}
	if flags.CLIVerbose != nil {
		if _, ok := os.LookupEnv("GOGEN_CLI_VERBOSE"); !ok {
			cfg.CLIVerbose = *flags.CLIVerbose
		}
	}
	if flags.WebBind != "" {
		if _, ok := os.LookupEnv("GOGEN_WEB_BIND"); !ok {
			cfg.WebBind = flags.WebBind
		}
	}

	return cfg
}

func mergeMCPServers(file FileConfig) []config.MCPServerConfig {
	if _, ok := os.LookupEnv("GOGEN_MCP_SERVERS"); ok {
		raw := os.Getenv("GOGEN_MCP_SERVERS")
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		var servers []config.MCPServerConfig
		if err := json.Unmarshal([]byte(raw), &servers); err != nil {
			log.Printf("warning: GOGEN_MCP_SERVERS is not a valid JSON array; ignoring it: %v", err)
			return nil
		}
		return servers
	}
	if file.MCPServers != nil {
		out := make([]config.MCPServerConfig, len(file.MCPServers))
		for i, s := range file.MCPServers {
			out[i] = config.MCPServerConfig{
				Name:            s.Name,
				Command:         s.Command,
				Args:            append([]string(nil), s.Args...),
				Env:             cloneStringMap(s.Env),
				InitTimeoutSecs: s.InitTimeoutSecs,
				ToolTimeoutSecs: s.ToolTimeoutSecs,
			}
		}
		return out
	}
	return nil
}

// mergeOpenAIProviders merges the registered OpenAI-compatible provider
// list: the GOGEN_OPENAI_PROVIDERS JSON env var wins when set, otherwise the
// file's openai_providers list is used (converted to config entries).
// An explicitly empty env value clears the list.
func mergeOpenAIProviders(file FileConfig) []config.OpenAIProviderConfig {
	if _, ok := os.LookupEnv("GOGEN_OPENAI_PROVIDERS"); ok {
		raw := os.Getenv("GOGEN_OPENAI_PROVIDERS")
		if strings.TrimSpace(raw) == "" {
			return nil
		}
		var providers []config.OpenAIProviderConfig
		if err := json.Unmarshal([]byte(raw), &providers); err != nil {
			log.Printf("warning: GOGEN_OPENAI_PROVIDERS is not a valid JSON array; ignoring it: %v", err)
			return nil
		}
		return providers
	}
	if file.OpenAIProviders != nil {
		out := make([]config.OpenAIProviderConfig, len(file.OpenAIProviders))
		for i, p := range file.OpenAIProviders {
			out[i] = config.OpenAIProviderConfig{
				Name:    p.Name,
				BaseURL: p.BaseURL,
				APIKey:  p.APIKey,
				Model:   p.Model,
			}
		}
		return out
	}
	return nil
}

// mergeCompactKeepRecentMessages merges the compact_keep_recent_messages
// setting, honoring the pre-rename spellings for back-compat:
// GOGEN_KEEP_RECENT_MESSAGES (env) and keep_recent_messages (file, already
// aliased onto CompactKeepRecentMessages by parseYAMLFrontMatter). The
// renamed key/env win when both are present; the legacy env var logs a
// deprecation warning instead of being silently dropped.
func mergeCompactKeepRecentMessages(file FileConfig, def int) int {
	if v, ok := envInt("GOGEN_COMPACT_KEEP_RECENT_MESSAGES", def); ok {
		return v
	}
	if v, ok := os.LookupEnv("GOGEN_KEEP_RECENT_MESSAGES"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			log.Printf("warning: GOGEN_KEEP_RECENT_MESSAGES=%q is not a valid integer; using default %d", v, def)
			return def
		}
		log.Printf("warning: GOGEN_KEEP_RECENT_MESSAGES is deprecated; use GOGEN_COMPACT_KEEP_RECENT_MESSAGES (value %d used)", n)
		return n
	}
	if file.CompactKeepRecentMessages != nil {
		return *file.CompactKeepRecentMessages
	}
	return def
}

// mergeCommandIdleTimeout merges the command_idle_timeout_secs setting,
// honoring the pre-rename GOGEN_COMMAND_TIMEOUT_SECS env var for back-compat
// (the file key command_timeout_secs is already aliased onto
// CommandIdleTimeoutSecs by parseYAMLFrontMatter). The renamed env var wins
// when both are set; the legacy env var logs a deprecation warning instead
// of being silently dropped. Precedence: env > file > default.
func mergeCommandIdleTimeout(file FileConfig, def int) int {
	if v, ok := envInt("GOGEN_COMMAND_IDLE_TIMEOUT_SECS", def); ok {
		return v
	}
	if v, ok := os.LookupEnv("GOGEN_COMMAND_TIMEOUT_SECS"); ok {
		n, err := strconv.Atoi(strings.TrimSpace(v))
		if err != nil {
			log.Printf("warning: GOGEN_COMMAND_TIMEOUT_SECS=%q is not a valid integer; using default %d", v, def)
			return def
		}
		log.Printf("warning: GOGEN_COMMAND_TIMEOUT_SECS is deprecated; use GOGEN_COMMAND_IDLE_TIMEOUT_SECS (value %d used)", n)
		return n
	}
	return config.Effective(file.CommandIdleTimeoutSecs, def)
}

func cloneStringMap(in map[string]string) map[string]string {
	if len(in) == 0 {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func envInt(envKey string, def int) (int, bool) {
	v, ok := os.LookupEnv(envKey)
	if !ok {
		return 0, false
	}
	if n, err := strconv.Atoi(v); err == nil {
		return n, true
	}
	log.Printf("warning: %s=%q is not a valid integer; using default %d", envKey, v, def)
	return def, true
}

func envFloat(envKey string, def float64) (float64, bool) {
	v, ok := os.LookupEnv(envKey)
	if !ok {
		return 0, false
	}
	if f, err := strconv.ParseFloat(v, 64); err == nil {
		return f, true
	}
	log.Printf("warning: %s=%q is not a valid number; using default %g", envKey, v, def)
	return def, true
}

func envBool(envKey string, def bool) (bool, bool) {
	v, ok := os.LookupEnv(envKey)
	if !ok {
		return false, false
	}
	on, ok := onoff.Parse(v)
	if !ok {
		log.Printf("warning: %s=%q is not a valid boolean; using default %t", envKey, v, def)
		return def, true
	}
	return on, true
}

// mergeString returns the env value when set, the file value when non-empty,
// or the default.
func mergeString(envKey string, fileVal, def string) string {
	if v, ok := os.LookupEnv(envKey); ok {
		return v
	}
	if fileVal != "" {
		return fileVal
	}
	return def
}

// mergeInt returns the env value when set, the file value when non-zero, or
// the default.
func mergeInt(envKey string, fileVal, def int) int {
	if v, ok := envInt(envKey, def); ok {
		return v
	}
	return config.Effective(fileVal, def)
}

// mergeIntKeepNegative is mergeInt for the one field where a negative value
// is meaningful: session_max_age_days -1 = "keep sessions forever" (the
// store's retention sentinel). config.Effective would map any value <= 0 to
// the default, which would silently convert a persisted -1 back to the
// default on the next start.
func mergeIntKeepNegative(envKey string, fileVal, def int) int {
	if v, ok := envInt(envKey, def); ok {
		return v
	}
	if fileVal < 0 {
		return fileVal
	}
	return config.Effective(fileVal, def)
}

// mergeIntOpt is like mergeInt for pointer fields: a non-nil pointer (an
// explicitly present key, including an explicit 0) wins over the default.
func mergeIntOpt(envKey string, fileVal *int, def int) int {
	if v, ok := envInt(envKey, def); ok {
		return v
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

// mergeFloatOpt is like mergeFloat for pointer fields.
func mergeFloatOpt(envKey string, fileVal *float64, def float64) float64 {
	if v, ok := envFloat(envKey, def); ok {
		return v
	}
	if fileVal != nil {
		return *fileVal
	}
	return def
}

// mergeBool returns the env value when set, the file value when true, or the
// default. Both boolean settings default to false, so an explicit false in
// the file is equivalent to absent.
func mergeBool(envKey string, fileVal, def bool) bool {
	if v, ok := envBool(envKey, def); ok {
		return v
	}
	if fileVal {
		return true
	}
	return def
}
