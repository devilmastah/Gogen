package automation

import (
	"strings"
	"testing"
)

// TestCappedTailKeepsNewestBytes pins the failure-detail contract: the
// writer must retain the LAST cap bytes (the most recent stderr output is
// the useful failure context), report how many older bytes were evicted
// ahead of the retained tail, and never let the retained window exceed cap.
func TestCappedTailKeepsNewestBytes(t *testing.T) {
	tests := []struct {
		name    string
		cap     int
		writes  []string
		want    string
		dropped int64
	}{
		{
			name:   "under cap passes through",
			cap:    16,
			writes: []string{"hello ", "world"},
			want:   "hello world",
		},
		{
			name:    "single oversized write keeps its tail",
			cap:     8,
			writes:  []string{"0123456789"},
			want:    "23456789",
			dropped: 2,
		},
		{
			name:    "bytes pushed out across several writes",
			cap:     4,
			writes:  []string{"ab", "cd", "ef"},
			want:    "cdef",
			dropped: 2,
		},
		{
			name:    "dropped count accumulates across evictions",
			cap:     4,
			writes:  []string{"ab", "cd", "ef", "gh"},
			want:    "efgh",
			dropped: 4,
		},
		{
			name:   "exactly cap retains everything",
			cap:    5,
			writes: []string{"abc", "de"},
			want:   "abcde",
		},
		{
			name:    "cap one keeps the final byte only",
			cap:     1,
			writes:  []string{"abc"},
			want:    "c",
			dropped: 2,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var tail cappedTail
			tail.cap = tc.cap
			for _, w := range tc.writes {
				n, err := tail.Write([]byte(w))
				if err != nil || n != len(w) {
					t.Fatalf("Write(%q) = %d, %v; want %d, nil", w, n, err, len(w))
				}
				if len(tail.tail) > tc.cap {
					t.Fatalf("retained %d bytes after Write(%q), cap %d", len(tail.tail), w, tc.cap)
				}
			}
			if tail.dropped != tc.dropped {
				t.Fatalf("dropped = %d, want %d", tail.dropped, tc.dropped)
			}
			got := tail.String()
			if tc.dropped == 0 {
				if got != tc.want {
					t.Fatalf("String() = %q, want %q", got, tc.want)
				}
				return
			}
			// Older bytes were evicted: the retained tail is a suffix and the
			// eviction count is announced ahead of it.
			if !strings.HasSuffix(got, tc.want) {
				t.Fatalf("String() = %q, want suffix %q", got, tc.want)
			}
			if !strings.Contains(got, "more bytes)") {
				t.Fatalf("String() = %q, want an eviction marker", got)
			}
		})
	}
}

// TestFireChildEnvDropsOverridingVars pins the environment hygiene: projectfile
// merge gives the environment priority over flags, so an exported
// GOGEN_WORKING_DIR made the fired child ignore --dir (and OPENAI_BASE_URL its
// --base-url). Everything else must survive, so exported keys/settings still
// apply to a scheduled run.
func TestFireChildEnvDropsOverridingVars(t *testing.T) {
	base := []string{
		"PATH=/usr/bin",
		"GOGEN_WORKING_DIR=/somewhere/else",
		"OPENAI_BASE_URL=https://other.example/v1",
		"OPENAI_API_KEY=sk-test",
		"GOGEN_MODEL=keep-me",
	}
	got := fireChildEnv(base, "EXTRA=1")
	joined := strings.Join(got, "\n")
	for _, banned := range []string{"GOGEN_WORKING_DIR=", "OPENAI_BASE_URL="} {
		if strings.Contains(joined, banned) {
			t.Fatalf("%s must be dropped from the child env:\n%s", banned, joined)
		}
	}
	for _, kept := range []string{"PATH=/usr/bin", "OPENAI_API_KEY=sk-test", "GOGEN_MODEL=keep-me", "EXTRA=1"} {
		if !strings.Contains(joined, kept) {
			t.Fatalf("%s must be kept:\n%s", kept, joined)
		}
	}
}
