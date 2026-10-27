package agent

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

// TestCallGraphWithTextSingleWalkFeedsBothHalves pins the text fallback of the
// call_graph tool after the two walks were merged into one: Callers (call-site
// pattern) and Callees (definition pattern) must come out EXACTLY as before —
// same names, files and line numbers, in the same order.
//
// The "one pass" property itself is structural, not observable from the
// result: a second walk over the same tree produces identical lists, so what
// this test pins is that merging the passes did not drop a match, reorder one,
// or change a line number.
func TestCallGraphWithTextSingleWalkFeedsBothHalves(t *testing.T) {
	tests := []struct {
		name        string
		files       map[string]string
		symbol      string
		wantCallers []FunctionRef
		wantCallees []FunctionRef
	}{
		{
			// Walk order is lexical (filepath.WalkDir sorts), so callers come
			// back caller1.go before caller2.go, and within a file in line
			// order: exactly the order the separate call-site walk produced.
			name: "callers across files plus the definition",
			files: map[string]string{
				"caller1.go": "package main\n\nfunc one() {\n\thelper()\n}\n",
				"caller2.go": "package main\n\nfunc two() {\n\thelper()\n\thelper()\n}\n",
				"shared.go":  "package main\n\nfunc helper() { a(); fmt.Println(\"x\"); b() }\n",
			},
			symbol: "helper",
			wantCallers: []FunctionRef{
				{Name: "helper", File: "caller1.go", Line: 4},
				{Name: "helper", File: "caller2.go", Line: 4},
				{Name: "helper", File: "caller2.go", Line: 5},
			},
			// Only the definition LINE is scanned (best-effort text search):
			// keywords/package names and the symbol itself are filtered out.
			wantCallees: []FunctionRef{
				{Name: "a", File: "shared.go", Line: 3},
				{Name: "Println", File: "shared.go", Line: 3},
				{Name: "b", File: "shared.go", Line: 3},
			},
		},
		{
			name: "caller with no definition has no callees",
			files: map[string]string{
				"use.go": "package main\n\nfunc caller() { outer() }\n",
			},
			symbol: "outer",
			wantCallers: []FunctionRef{
				{Name: "outer", File: "use.go", Line: 3},
			},
			wantCallees: nil,
		},
		{
			// Definition and call site in the SAME file: the merged pass must
			// classify each line independently (the definition line is not a
			// call site, the call line is not a definition).
			name: "definition and caller in the same file",
			files: map[string]string{
				"same.go": "package main\n\nfunc helper() { inner() }\n\nfunc useHelper() { helper() }\n",
			},
			symbol: "helper",
			wantCallers: []FunctionRef{
				{Name: "helper", File: "same.go", Line: 5},
			},
			wantCallees: []FunctionRef{
				{Name: "inner", File: "same.go", Line: 3},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, content := range tc.files {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			exec := NewExecutor(dir)
			result, err := exec.callGraphWithText(context.Background(), dir, "", "", tc.symbol, &walkSkips{})
			if err != nil {
				t.Fatalf("callGraphWithText: %v", err)
			}
			if result.Method != "text" {
				t.Fatalf("method = %q, want text", result.Method)
			}
			assertFunctionRefs(t, "Callers", result.Callers, tc.wantCallers)
			assertFunctionRefs(t, "Callees", result.Callees, tc.wantCallees)
		})
	}
}

// assertFunctionRefs fails unless got matches want element by element (order
// included), listing both slices on failure.
func assertFunctionRefs(t *testing.T, label string, got, want []FunctionRef) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%s = %+v (len %d), want %+v (len %d)", label, got, len(got), want, len(want))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("%s[%d] = %+v, want %+v (full: %+v)", label, i, got[i], want[i], got)
		}
	}
}
