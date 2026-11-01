package projectfile

import (
	"os"
	"reflect"
	"strconv"
	"strings"
	"testing"

	"gogen/internal/config"
)

// The tests in this file are the C3 drift guard. Every configuration setting is
// declared once (settings.go) and the merge + save paths are generated from it,
// so these tests assert the three things that used to go wrong by hand:
//
//   - a setting merged from the file but never written back (a save silently
//     erased it — that is how web_fetch, web_search*, web_allowed_domains,
//     web_fetch_mode and debug_compare_messages were lost);
//   - a setting written under a key that is not the one it is read from;
//   - an env var that no longer matches the documented name.

// TestSettingsCoverEveryField asserts the table is complete in both
// directions: every FileConfig YAML key is declared exactly once, and every
// field of the saved projection (configYAML) is produced by a row.
func TestSettingsCoverEveryField(t *testing.T) {
	byFile := map[string]int{}
	byWriter := map[string]int{}
	byKey := map[string]int{}
	for _, s := range settings {
		if s.key == "" {
			t.Errorf("row for %q has no yaml key", s.file)
		}
		if n := byKey[s.key]; n > 0 {
			t.Errorf("yaml key %q declared twice", s.key)
		}
		byKey[s.key]++
		if s.file != "" {
			if byFile[s.file] > 0 {
				t.Errorf("FileConfig.%s declared twice", s.file)
			}
			byFile[s.file]++
		}
		if s.writer != "" {
			if byWriter[s.writer] > 0 {
				t.Errorf("configYAML.%s produced twice", s.writer)
			}
			byWriter[s.writer]++
		}
	}

	fileType := reflect.TypeOf(FileConfig{})
	for i := 0; i < fileType.NumField(); i++ {
		f := fileType.Field(i)
		key := strings.Split(f.Tag.Get("yaml"), ",")[0]
		if f.PkgPath != "" || key == "" || key == "-" {
			continue
		}
		if byFile[f.Name] == 0 {
			t.Errorf("FileConfig.%s (yaml %q) has no settings row: it can be read but is silently dropped on save", f.Name, key)
		}
		if byKey[key] != 1 {
			t.Errorf("yaml key %q: want exactly one settings row, got %d", key, byKey[key])
		}
	}

	writerType := reflect.TypeOf(configYAML{})
	for i := 0; i < writerType.NumField(); i++ {
		f := writerType.Field(i)
		if f.PkgPath != "" {
			continue
		}
		if byWriter[f.Name] == 0 {
			t.Errorf("configYAML.%s is never filled: a saved config would drop it", f.Name)
		}
	}
}

// TestSettingsCoverConfigFields asserts every runtime field is either merged
// from the file table or explicitly declared as derived-per-process, so a new
// setting cannot silently stay at its zero value.
func TestSettingsCoverConfigFields(t *testing.T) {
	merged := map[string]bool{}
	for _, s := range settings {
		if s.cfg != "" {
			merged[s.cfg] = true
		}
	}
	derived := map[string]bool{}
	for _, name := range settingsNotInFiles {
		derived[name] = true
	}
	typ := reflect.TypeOf(config.Config{})
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.PkgPath != "" {
			continue
		}
		if !merged[f.Name] && !derived[f.Name] {
			t.Errorf("config.Config.%s has no settings row and is not in settingsNotInFiles: it would always be zero", f.Name)
		}
		if merged[f.Name] && derived[f.Name] {
			t.Errorf("config.Config.%s is both merged and declared derived", f.Name)
		}
	}
}

// TestSettingsTypesAgree asserts the three views of a setting have the same Go
// type (a mismatch would panic at runtime in the reflective merge) and that the
// YAML key it is read from is the key it is written to.
func TestSettingsTypesAgree(t *testing.T) {
	cfgT := reflect.TypeOf(config.Config{})
	fileT := reflect.TypeOf(FileConfig{})
	writerT := reflect.TypeOf(configYAML{})
	for _, s := range settings {
		if s.kind == kLegacy {
			continue
		}
		cf, ok := cfgT.FieldByName(s.cfg)
		if !ok {
			t.Errorf("%s: config.Config has no field %s", s.key, s.cfg)
			continue
		}
		if _, ok := fileT.FieldByName(s.file); !ok && s.kind != kMCPServers && s.kind != kOpenAIProviders {
			t.Errorf("%s: FileConfig has no field %s", s.key, s.file)
		}
		if s.writer == "" {
			t.Errorf("%s: no writer field: it can be read but never saved", s.key)
			continue
		}
		wf, ok := writerT.FieldByName(s.writer)
		if !ok {
			t.Errorf("%s: configYAML has no field %s", s.key, s.writer)
			continue
		}
		if cf.Type != wf.Type {
			// code_index_chunk_overlap is an int in config and a *int in the
			// saved file so an explicit 0 round-trips (see writeSettings).
			intPtr := cf.Type.Kind() == reflect.Int && wf.Type.Kind() == reflect.Pointer && wf.Type.Elem().Kind() == reflect.Int
			if !intPtr {
				t.Errorf("%s: config field type %s != writer field type %s", s.key, cf.Type, wf.Type)
			}
		}
		if got := strings.Split(wf.Tag.Get("yaml"), ",")[0]; got != s.key {
			t.Errorf("%s: written to yaml key %q, want %q", s.key, got, s.key)
		}
		// The file key must be the one written, so a save/reload is a no-op.
		if ff, ok := fileT.FieldByName(s.file); ok && s.kind != kMCPServers && s.kind != kOpenAIProviders {
			if got := strings.Split(ff.Tag.Get("yaml"), ",")[0]; got != s.key {
				t.Errorf("%s: read from yaml key %q, want %q", s.key, got, s.key)
			}
		}
	}
}

// TestSettingsEnvVarsAreRead asserts the env var named by each row actually
// reaches the merged config. The names are a user-facing interface (scripts set
// them), so a rename would be a silent break; this fails instead.
func TestSettingsEnvVarsAreRead(t *testing.T) {
	for _, s := range settings {
		if s.env == "" || s.kind == kLegacy {
			continue
		}
		t.Run(s.key, func(t *testing.T) {
			requireSettingRoundTrips(t, s, viaEnv)
		})
	}
}

// TestSettingsFileValuesAreReadAndWritten is the drift test that matters most:
// a non-default value set in the file must survive merge -> save -> parse ->
// merge. A setting that is merged but not written fails here (which is exactly
// how the web_fetch group and debug_compare_messages were being erased).
func TestSettingsFileValuesAreReadAndWritten(t *testing.T) {
	// The four bespoke-merge settings (two renamed keys with legacy
	// aliases, the two JSON list settings) have their own tests:
	// TestMergeCompactKeepRecentMessages*, TestMergeCommandIdleTimeout*,
	// TestMCPServers*, TestOpenAIProviders*.
	bespoke := map[kind]bool{kKeepRecent: true, kCommandIdleTimeout: true, kMCPServers: true, kOpenAIProviders: true}
	for _, s := range settings {
		if s.kind == kLegacy || bespoke[s.kind] {
			continue
		}
		t.Run(s.key, func(t *testing.T) {
			requireSettingRoundTrips(t, s, viaFile)
		})
	}
}

// TestSettingsOmitDefault pins the omission rule per row: a setting marked
// omitDefault must not bake the built-in default into a saved file, and a
// setting without the mark keeps its (non-default) value.
func TestSettingsOmitDefault(t *testing.T) {
	def := config.Defaults()
	for _, s := range settings {
		if s.kind == kLegacy || s.writer == "" || s.writeSpecial || s.secret {
			continue // writeSpecial rows and secrets have their own tests
		}
		t.Run(s.key, func(t *testing.T) {
			body, err := buildConfigYAML(&def, WriteOptions{})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if s.omitDefault && yamlHasKey(body, s.key) {
				t.Fatalf("%s equals its default (%v) but was baked into the saved file", s.key, fieldValue(def, s.cfg))
			}
			// A non-default value is always written.
			cfg := def
			setSettingValue(t, &cfg, s)
			body, err = buildConfigYAML(&cfg, WriteOptions{IncludeSecrets: true})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if !yamlHasKey(body, s.key) {
				t.Fatalf("%s set to a non-default value is missing from the saved file:\n%s", s.key, body)
			}
		})
	}
}

// TestSettingsSecretsRequireOptIn pins the secret gate: a secret value is
// written only with IncludeSecrets, and never otherwise.
func TestSettingsSecretsRequireOptIn(t *testing.T) {
	for _, s := range settings {
		if !s.secret {
			continue
		}
		t.Run(s.key, func(t *testing.T) {
			cfg := config.Defaults()
			setSettingValue(t, &cfg, s)
			without, err := buildConfigYAML(&cfg, WriteOptions{})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if yamlHasKey(without, s.key) || strings.Contains(without, fieldValueString(cfg, s.cfg)) {
				t.Fatalf("%s written without IncludeSecrets:\n%s", s.key, without)
			}
			with, err := buildConfigYAML(&cfg, WriteOptions{IncludeSecrets: true})
			if err != nil {
				t.Fatalf("build: %v", err)
			}
			if !yamlHasKey(with, s.key) {
				t.Fatalf("%s missing with IncludeSecrets:\n%s", s.key, with)
			}
		})
	}
}

// viaEnv / viaFile select where the round-trip takes its value from.
const (
	viaEnv = iota
	viaFile
)

// unsetEnvFor clears s's env var for the duration of the test, so a developer
// shell that exports it cannot mask the file path (t.Setenv cannot unset, and
// an empty value still wins over the file).
func unsetEnvFor(t *testing.T, s setting) {
	t.Helper()
	if s.env == "" {
		return
	}
	old, had := os.LookupEnv(s.env)
	if err := os.Unsetenv(s.env); err != nil {
		t.Fatalf("unset %s: %v", s.env, err)
	}
	t.Cleanup(func() {
		if had {
			os.Setenv(s.env, old)
		}
	})
}

// yamlHasKey reports whether the saved document contains the key. The first
// line of the document has no leading newline, so both positions are checked.
func yamlHasKey(body, key string) bool {
	return strings.HasPrefix(body, key+":") || strings.Contains(body, "\n"+key+":")
}

// requireSettingRoundTrips sets one setting, saves the config, parses it back
// and merges it, then asserts the value survived.
func requireSettingRoundTrips(t *testing.T, s setting, mode int) {
	t.Helper()
	var file FileConfig
	if mode == viaEnv {
		// The env var must win over the default and over an empty file.
		cfg := config.Defaults()
		value := distinctValue(t, cfg, s, mode)
		t.Setenv(s.env, value)
		merged := Merge(&ProjectFile{Config: file}, FlagOverrides{})
		requireSettingValue(t, merged, s, value, "env")
		return
	}

	// File value: merge picks it up, save writes it, reload merges it again.
	unsetEnvFor(t, s)
	cfg := config.Defaults()
	value := distinctValue(t, cfg, s, mode)
	requireSettingValue(t, setSettingValueViaFile(t, s, value), s, value, "file (merge)")
	body, err := buildConfigYAML(setSettingValue(t, &cfg, s), WriteOptions{IncludeSecrets: true})
	if err != nil {
		t.Fatalf("build: %v", err)
	}
	if !yamlHasKey(body, s.key) {
		t.Fatalf("%s not written to the saved config:\n%s", s.key, body)
	}
	parsed, err := parseYAMLConfig(body)
	if err != nil {
		t.Fatalf("parse saved config: %v", err)
	}
	requireSettingValue(t, Merge(&ProjectFile{Config: parsed}, FlagOverrides{}), s, value, "file (round-trip)")
}

// distinctValue returns a value for s that differs from its current (default)
// value, as the string the file/env path carries.
func distinctValue(t *testing.T, cfg config.Config, s setting, mode int) string {
	t.Helper()
	cur := reflect.ValueOf(cfg).FieldByName(s.cfg)
	switch s.kind {
	case kString:
		// code_index_watch omits every value except an explicit off, so the
		// generic round trip has to use that one persisted spelling.
		if s.key == "code_index_watch" {
			return "off"
		}
		// code_index_rerank omits every value except an explicit on.
		if s.key == "code_index_rerank" {
			return "on"
		}
		v := "test-" + s.key
		if v == cur.String() {
			v += "-2"
		}
		return v
	case kInt, kIntPtr, kIntKeepNegative, kKeepRecent, kCommandIdleTimeout:
		return strconv.Itoa(int(cur.Int()) + 7)
	case kFloatPtr:
		return strconv.FormatFloat(cur.Float()+0.25, 'f', 2, 64)
	case kBool:
		if cur.Bool() {
			return "off"
		}
		return "on"
	}
	t.Fatalf("%s: no distinct value helper for kind %v", s.key, s.kind)
	return ""
}

// requireSettingValue asserts the merged config carries want for s.
func requireSettingValue(t *testing.T, cfg *config.Config, s setting, want, where string) {
	t.Helper()
	got := reflect.ValueOf(cfg).Elem().FieldByName(s.cfg)
	var gotStr string
	switch s.kind {
	case kString:
		gotStr = got.String()
	case kInt, kIntPtr, kIntKeepNegative, kKeepRecent, kCommandIdleTimeout:
		gotStr = strconv.Itoa(int(got.Int()))
	case kFloatPtr:
		gotStr = strconv.FormatFloat(got.Float(), 'f', 2, 64)
	case kBool:
		gotStr = "off"
		if got.Bool() {
			gotStr = "on"
		}
	default:
		t.Fatalf("%s: unhandled kind %v", s.key, s.kind)
	}
	if gotStr != want {
		t.Fatalf("%s via %s = %q, want %q", s.key, where, gotStr, want)
	}
}

// setSettingValueViaFile puts the value in a FileConfig and merges it.
func setSettingValueViaFile(t *testing.T, s setting, value string) *config.Config {
	t.Helper()
	file := FileConfig{}
	fv := reflect.ValueOf(&file).Elem().FieldByName(s.file)
	switch s.kind {
	case kString:
		fv.SetString(value)
	case kInt, kIntKeepNegative, kKeepRecent, kCommandIdleTimeout:
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("atoi %q: %v", value, err)
		}
		fv.SetInt(int64(n))
	case kIntPtr:
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("atoi %q: %v", value, err)
		}
		fv.Set(reflect.ValueOf(&n))
	case kFloatPtr:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("atof %q: %v", value, err)
		}
		fv.Set(reflect.ValueOf(&f))
	case kBool:
		fv.SetBool(value == "on")
	default:
		t.Fatalf("%s: unhandled kind %v", s.key, s.kind)
	}
	return Merge(&ProjectFile{Config: file}, FlagOverrides{})
}

// setSettingValue puts a distinct non-default value into cfg, for the save
// direction.
func setSettingValue(t *testing.T, cfg *config.Config, s setting) *config.Config {
	t.Helper()
	value := distinctValue(t, *cfg, s, viaFile)
	dst := reflect.ValueOf(cfg).Elem().FieldByName(s.cfg)
	switch s.kind {
	case kString:
		dst.SetString(value)
	case kInt, kIntPtr, kIntKeepNegative, kKeepRecent, kCommandIdleTimeout:
		n, err := strconv.Atoi(value)
		if err != nil {
			t.Fatalf("atoi %q: %v", value, err)
		}
		dst.SetInt(int64(n))
	case kFloatPtr:
		f, err := strconv.ParseFloat(value, 64)
		if err != nil {
			t.Fatalf("atof %q: %v", value, err)
		}
		dst.SetFloat(f)
	case kBool:
		dst.SetBool(value == "on")
	default:
		t.Fatalf("%s: unhandled kind %v", s.key, s.kind)
	}
	return cfg
}

// fieldValue / fieldValueString give a test-readable view of a Config field.
func fieldValue(cfg config.Config, name string) any {
	return reflect.ValueOf(cfg).FieldByName(name).Interface()
}

func fieldValueString(cfg config.Config, name string) string {
	v := reflect.ValueOf(cfg).FieldByName(name)
	if v.Kind() == reflect.String {
		return v.String()
	}
	return ""
}
