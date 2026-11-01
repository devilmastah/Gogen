package projectfile

import (
	"strings"
	"testing"

	"gogen/internal/config"
)

func TestCodeIndexSettings(t *testing.T) {
	def := config.Defaults()
	if def.CodeIndexEnabled() {
		t.Fatal("code index defaults on")
	}
	lines, overlap := def.CodeIndexChunk()
	if lines != 40 || overlap != 5 {
		t.Fatalf("default chunk = %d/%d", lines, overlap)
	}
	body, err := buildConfigYAML(&def, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"code_index:", "code_index_provider:", "code_index_model:", "code_index_chunk_lines:", "code_index_chunk_overlap:"} {
		if strings.Contains(body, key) {
			t.Fatalf("default config wrote %s:\n%s", key, body)
		}
	}

	on := def
	on.CodeIndex = "on"
	on.CodeIndexProvider = "embed"
	on.CodeIndexModel = "bge"
	on.CodeIndexChunkLines = 32
	on.CodeIndexChunkOverlap = 4
	body, err = buildConfigYAML(&on, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseYAMLConfig(body)
	if err != nil {
		t.Fatal(err)
	}
	got := Merge(&ProjectFile{Config: parsed}, FlagOverrides{})
	if !got.CodeIndexEnabled() || got.CodeIndexProvider != "embed" || got.CodeIndexModel != "bge" {
		t.Fatalf("round trip = %+v", got)
	}
	gotLines, gotOverlap := got.CodeIndexChunk()
	if gotLines != 32 || gotOverlap != 4 {
		t.Fatalf("chunk = %d/%d", gotLines, gotOverlap)
	}

	unknown := Merge(&ProjectFile{Config: FileConfig{CodeIndex: "maybe"}}, FlagOverrides{})
	if unknown.CodeIndexEnabled() {
		t.Fatal("unknown code_index is on")
	}
}

func TestCodeIndexChunkBounds(t *testing.T) {
	cases := []struct {
		name           string
		lines, overlap int
		wantL, wantO   int
	}{
		{name: "zero lines", lines: 0, overlap: 5, wantL: 40, wantO: 5},
		{name: "nine", lines: 9, overlap: 5, wantL: 40, wantO: 5},
		{name: "too big", lines: 201, overlap: 5, wantL: 40, wantO: 5},
		{name: "overlap equals chunk", lines: 40, overlap: 40, wantL: 40, wantO: 5},
		{name: "overlap zero", lines: 40, overlap: 0, wantL: 40, wantO: 0},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Defaults()
			cfg.CodeIndexChunkLines = tc.lines
			cfg.CodeIndexChunkOverlap = tc.overlap
			gotL, gotO := cfg.CodeIndexChunk()
			if gotL != tc.wantL || gotO != tc.wantO {
				t.Fatalf("got %d/%d, want %d/%d", gotL, gotO, tc.wantL, tc.wantO)
			}
		})
	}

	zero := 0
	file := FileConfig{CodeIndexChunkLines: 201, CodeIndexChunkOverlap: &zero}
	got := Merge(&ProjectFile{Config: file}, FlagOverrides{})
	lines, overlap := got.CodeIndexChunk()
	if lines != 40 || overlap != 0 {
		t.Fatalf("file bounds = %d/%d, want 40/0", lines, overlap)
	}
	body, err := buildConfigYAML(got, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "code_index_chunk_lines:") {
		t.Fatalf("out-of-range chunk was saved:\n%s", body)
	}
	if !strings.Contains(body, "code_index_chunk_overlap: 0") {
		t.Fatalf("overlap 0 was not saved:\n%s", body)
	}
}

func TestCodeIndexWatchSaved(t *testing.T) {
	def := config.Defaults()
	if !def.CodeIndexWatchEnabled() {
		t.Fatal("unset watch is off")
	}
	body, err := buildConfigYAML(&def, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "code_index_watch:") {
		t.Fatalf("empty watch was saved:\n%s", body)
	}
	on := def
	on.CodeIndexWatch = "on"
	body, err = buildConfigYAML(&on, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "code_index_watch:") {
		t.Fatalf("on watch was saved:\n%s", body)
	}
	unknown := def
	unknown.CodeIndexWatch = "maybe"
	if !unknown.CodeIndexWatchEnabled() {
		t.Fatal("unknown watch is off")
	}
	body, err = buildConfigYAML(&unknown, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "code_index_watch:") {
		t.Fatalf("unknown watch was saved:\n%s", body)
	}

	off := def
	off.CodeIndexWatch = "off"
	body, err = buildConfigYAML(&off, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseYAMLConfig(body)
	if err != nil {
		t.Fatal(err)
	}
	got := Merge(&ProjectFile{Config: parsed}, FlagOverrides{})
	if got.CodeIndexWatch != "off" || got.CodeIndexWatchEnabled() {
		t.Fatalf("off watch = %q enabled %v", got.CodeIndexWatch, got.CodeIndexWatchEnabled())
	}

	t.Setenv("GOGEN_CODE_INDEX_WATCH", "off")
	fromFile := Merge(&ProjectFile{Config: FileConfig{CodeIndexWatch: "on"}}, FlagOverrides{})
	if fromFile.CodeIndexWatchEnabled() {
		t.Fatal("env off did not beat the file")
	}
}

func TestCodeIndexEnvOverFile(t *testing.T) {
	t.Setenv("GOGEN_CODE_INDEX", "on")
	t.Setenv("GOGEN_CODE_INDEX_PROVIDER", "from-env")
	t.Setenv("GOGEN_CODE_INDEX_MODEL", "env-model")
	t.Setenv("GOGEN_CODE_INDEX_CHUNK_LINES", "20")
	t.Setenv("GOGEN_CODE_INDEX_CHUNK_OVERLAP", "3")
	four := 4
	file := FileConfig{
		CodeIndex: "off", CodeIndexProvider: "from-file", CodeIndexModel: "file-model",
		CodeIndexChunkLines: 32, CodeIndexChunkOverlap: &four,
	}
	got := Merge(&ProjectFile{Config: file}, FlagOverrides{})
	if !got.CodeIndexEnabled() || got.CodeIndexProvider != "from-env" || got.CodeIndexModel != "env-model" {
		t.Fatalf("env did not win: %+v", got)
	}
	lines, overlap := got.CodeIndexChunk()
	if lines != 20 || overlap != 3 {
		t.Fatalf("chunk = %d/%d", lines, overlap)
	}
}

func TestCodeIndexRerankSaved(t *testing.T) {
	def := config.Defaults()
	if def.CodeIndexRerankEnabled() {
		t.Fatal("unset rerank is on")
	}
	body, err := buildConfigYAML(&def, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "code_index_rerank:") {
		t.Fatalf("default config wrote rerank:\n%s", body)
	}

	off := def
	off.CodeIndexRerank = "off"
	body, err = buildConfigYAML(&off, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "code_index_rerank:") {
		t.Fatalf("off rerank was written:\n%s", body)
	}

	unknown := def
	unknown.CodeIndexRerank = "maybe"
	if unknown.CodeIndexRerankEnabled() {
		t.Fatal("unknown rerank is on")
	}
	body, err = buildConfigYAML(&unknown, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "code_index_rerank:") {
		t.Fatalf("unknown rerank was written:\n%s", body)
	}

	on := def
	on.CodeIndexRerank = "on"
	body, err = buildConfigYAML(&on, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseYAMLConfig(body)
	if err != nil {
		t.Fatal(err)
	}
	got := Merge(&ProjectFile{Config: parsed}, FlagOverrides{})
	if got.CodeIndexRerank != "on" || !got.CodeIndexRerankEnabled() {
		t.Fatalf("on rerank = %q enabled %v", got.CodeIndexRerank, got.CodeIndexRerankEnabled())
	}

	t.Setenv("GOGEN_CODE_INDEX_RERANK", "on")
	fromFile := Merge(&ProjectFile{Config: FileConfig{CodeIndexRerank: "off"}}, FlagOverrides{})
	if !fromFile.CodeIndexRerankEnabled() {
		t.Fatal("env on did not beat the file")
	}
}

func TestCodeIndexRerankTargetSaved(t *testing.T) {
	def := config.Defaults()
	body, err := buildConfigYAML(&def, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(body, "code_index_rerank_provider:") || strings.Contains(body, "code_index_rerank_model:") {
		t.Fatalf("default config wrote a rerank target:\n%s", body)
	}

	set := def
	set.CodeIndexRerankProvider = "rerank"
	set.CodeIndexRerankModel = "qwen3-reranker"
	body, err = buildConfigYAML(&set, WriteOptions{})
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := parseYAMLConfig(body)
	if err != nil {
		t.Fatal(err)
	}
	got := Merge(&ProjectFile{Config: parsed}, FlagOverrides{})
	if got.CodeIndexRerankProvider != "rerank" || got.CodeIndexRerankModel != "qwen3-reranker" {
		t.Fatalf("saved target = %q %q", got.CodeIndexRerankProvider, got.CodeIndexRerankModel)
	}

	t.Setenv("GOGEN_CODE_INDEX_RERANK_PROVIDER", "from-env")
	t.Setenv("GOGEN_CODE_INDEX_RERANK_MODEL", "env-model")
	fromFile := Merge(&ProjectFile{Config: FileConfig{
		CodeIndexRerankProvider: "from-file",
		CodeIndexRerankModel:    "file-model",
	}}, FlagOverrides{})
	if fromFile.CodeIndexRerankProvider != "from-env" || fromFile.CodeIndexRerankModel != "env-model" {
		t.Fatalf("env did not win: %q %q", fromFile.CodeIndexRerankProvider, fromFile.CodeIndexRerankModel)
	}
}
