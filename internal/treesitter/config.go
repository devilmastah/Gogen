package treesitter

import (
	"os"
	"strings"
	"sync"
)

var (
	cfgMu       sync.RWMutex
	cfgEnabled  *bool
	cfgLangs    string
	cfgLangsSet bool
)

// Configure applies runtime tree-sitter settings from merged config.
func Configure(enabled bool, langs string) {
	cfgMu.Lock()
	defer cfgMu.Unlock()
	cfgEnabled = &enabled
	cfgLangs = langs
	cfgLangsSet = true
}

// Enabled reports whether tree-sitter syntax checks are active.
func Enabled() bool {
	cfgMu.RLock()
	if cfgEnabled != nil {
		e := *cfgEnabled
		cfgMu.RUnlock()
		return e
	}
	cfgMu.RUnlock()
	v := strings.ToLower(strings.TrimSpace(os.Getenv("GOGEN_TREESITTER")))
	if v == "off" || v == "0" || v == "false" {
		return false
	}
	return true
}

// allowedLangsCache memoizes the parsed allowlist keyed by the raw value it was
// built from (the configured field or the environment variable). langNameForPath
// and friends call this per FILE, and rebuilding the map per file was pure
// garbage; keying on the raw string keeps a later SetConfig/env change — and
// tests that set the variable per case — exact.
var (
	allowedLangsMu    sync.Mutex
	allowedLangsRaw   string
	allowedLangsSet   map[string]struct{}
	allowedLangsValid bool
)

// allowedLangs returns the parsed language allowlist (nil = all allowed). The
// returned map is SHARED and read-only: callers must not mutate it.
func allowedLangs() map[string]struct{} {
	cfgMu.RLock()
	var raw string
	if cfgLangsSet {
		raw = strings.TrimSpace(cfgLangs)
		cfgMu.RUnlock()
	} else {
		cfgMu.RUnlock()
		raw = strings.TrimSpace(os.Getenv("GOGEN_TREESITTER_LANGS"))
	}
	allowedLangsMu.Lock()
	defer allowedLangsMu.Unlock()
	if allowedLangsValid && raw == allowedLangsRaw {
		return allowedLangsSet
	}
	parsed := parseAllowedLangs(raw)
	allowedLangsRaw, allowedLangsSet, allowedLangsValid = raw, parsed, true
	return parsed
}

// parseAllowedLangs splits a comma-separated allowlist.
func parseAllowedLangs(raw string) map[string]struct{} {
	if raw == "" {
		return nil
	}
	out := make(map[string]struct{})
	for _, part := range strings.Split(raw, ",") {
		name := strings.ToLower(strings.TrimSpace(part))
		if name != "" {
			out[name] = struct{}{}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func langAllowed(name string, allowed map[string]struct{}) bool {
	if allowed == nil {
		return true
	}
	_, ok := allowed[strings.ToLower(name)]
	return ok
}
