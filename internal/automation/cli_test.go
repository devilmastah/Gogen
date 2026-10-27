package automation

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// runCLICapture executes a CLI command against a fresh temp store and
// returns the stdout body.
func runCLICapture(t *testing.T, store *Store, args ...string) string {
	t.Helper()
	var out bytes.Buffer
	if err := runCLI(args, store, &out); err != nil {
		t.Fatalf("runCLI(%v): %v", args, err)
	}
	return out.String()
}

func TestCLIListJSON(t *testing.T) {
	st, _ := newTestStore(t)
	a := Automation{
		Title: "Nightly triage", Prompt: "triage", WorkingDir: "/tmp/proj",
		Schedule: Schedule{Kind: KindDaily, Time: "22:00", Timezone: "America/New_York"}, Enabled: true,
	}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	out := runCLICapture(t, st, "ls", "--json")
	var decoded struct {
		Automations []Automation `json:"automations"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("ls --json is not machine-readable JSON: %v\n%s", err, out)
	}
	if len(decoded.Automations) != 1 {
		t.Fatalf("ls --json returned %d automations, want 1", len(decoded.Automations))
	}
	got := decoded.Automations[0]
	if got.Title != a.Title || got.Schedule.Kind != KindDaily || got.Schedule.Time != "22:00" ||
		got.Schedule.Timezone != "America/New_York" || !got.Enabled {
		t.Fatalf("ls --json record drifted: %+v", got)
	}
}

func TestCLIListHuman(t *testing.T) {
	st, _ := newTestStore(t)
	out := runCLICapture(t, st, "ls")
	if !strings.Contains(out, "No automations") {
		t.Fatalf("empty ls should hint at create, got %q", out)
	}
	a := Automation{Title: "Hourly check", Prompt: "p", WorkingDir: "/tmp",
		Schedule: Schedule{Kind: KindHourly, Minute: 15}, Enabled: true}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	out = runCLICapture(t, st, "ls")
	for _, want := range []string{"Hourly check", "hourly :15 local", "on", a.ID} {
		if !strings.Contains(out, want) {
			t.Fatalf("ls output missing %q:\n%s", want, out)
		}
	}
}

func TestCLICreateAllScheduleSelectors(t *testing.T) {
	cases := []struct {
		name     string
		args     []string
		wantKind string
		wantTime string
		wantMin  int
		wantDays []int
	}{
		{"once", []string{"--at", "2026-08-10T09:00:00Z"}, KindOnce, "", 0, nil},
		{"once local", []string{"--at", "2026-08-10 09:00", "--timezone", "UTC"}, KindOnce, "", 0, nil},
		{"daily", []string{"--daily", "--time", "07:15"}, KindDaily, "07:15", 0, nil},
		{"hourly", []string{"--hourly", "--minute", "45"}, KindHourly, "", 45, nil},
		{"weekdays", []string{"--weekdays", "--time", "08:00"}, KindWeekdays, "08:00", 0, nil},
		{"weekly", []string{"--weekly", "1,3,5", "--time", "09:30"}, KindWeekly, "09:30", 0, []int{1, 3, 5}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			st, cursor := newTestStore(t)
			args := append([]string{"create", "My job", "--prompt", "do it", "--dir", "/tmp/proj"}, tc.args...)
			runCLICapture(t, st, args...)
			autos := st.List()
			if len(autos) != 1 {
				t.Fatalf("create stored %d automations, want 1", len(autos))
			}
			got := autos[0]
			if got.Schedule.Kind != tc.wantKind || got.Schedule.Time != tc.wantTime ||
				got.Schedule.Minute != tc.wantMin {
				t.Fatalf("schedule = %+v, want kind %s time %q minute %d", got.Schedule, tc.wantKind, tc.wantTime, tc.wantMin)
			}
			if tc.wantDays != nil {
				if len(got.Schedule.Weekdays) != len(tc.wantDays) {
					t.Fatalf("weekdays = %v, want %v", got.Schedule.Weekdays, tc.wantDays)
				}
				for i := range tc.wantDays {
					if got.Schedule.Weekdays[i] != tc.wantDays[i] {
						t.Fatalf("weekdays = %v, want %v", got.Schedule.Weekdays, tc.wantDays)
					}
				}
			}
			// next_run_at is computed from the store clock (the test
			// cursor), never from wall time.
			if got.NextRunAt == nil {
				t.Fatalf("create did not compute next_run_at: %+v", got)
			}
			if !got.NextRunAt.After(cursor.t) {
				t.Fatalf("next_run_at = %v, want after the store clock %v", got.NextRunAt, cursor.t)
			}
		})
	}
}

func TestCLICreateRejectsScheduleConflicts(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no selector", []string{"create", "t", "--prompt", "p"}},
		{"two selectors", []string{"create", "t", "--prompt", "p", "--daily", "--hourly"}},
		{"at and weekly", []string{"create", "t", "--prompt", "p", "--at", "2026-08-10T09:00:00Z", "--weekly", "1"}},
		{"missing prompt", []string{"create", "t", "--daily"}},
		{"bad weekdays value", []string{"create", "t", "--prompt", "p", "--weekly", "8"}},
		{"bad timezone", []string{"create", "t", "--prompt", "p", "--daily", "--timezone", "Mars/Base"}},
		{"bad time", []string{"create", "t", "--prompt", "p", "--daily", "--time", "9am"}},
		{"bad at", []string{"create", "t", "--prompt", "p", "--at", "whenever"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newTestStore(t)
			var out bytes.Buffer
			err := runCLI(tc.args, st, &out)
			if err == nil {
				t.Fatalf("create accepted invalid input: %v", tc.args)
			}
			if got := len(st.List()); got != 0 {
				t.Fatalf("rejected create stored %d rows", got)
			}
		})
	}
}

func TestCLIEnableDisableGetJSON(t *testing.T) {
	st, _ := newTestStore(t)
	a := Automation{Title: "Nightly", Prompt: "p", WorkingDir: "/tmp",
		Schedule: Schedule{Kind: KindDaily, Time: "09:00"}, Enabled: true}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}

	out := runCLICapture(t, st, "disable", a.ID)
	if !strings.Contains(out, "Disabled") {
		t.Fatalf("disable output: %q", out)
	}
	got, _ := st.Get(a.ID)
	if got.Enabled {
		t.Fatal("disable did not persist")
	}
	// get --json reflects the disabled state.
	out = runCLICapture(t, st, "get", a.ID, "--json")
	var decoded struct {
		Automation Automation `json:"automation"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("get --json not machine-readable: %v\n%s", err, out)
	}
	if decoded.Automation.Enabled {
		t.Fatalf("get --json shows enabled after disable: %+v", decoded.Automation)
	}

	out = runCLICapture(t, st, "enable", a.ID)
	if !strings.Contains(out, "Enabled") || !strings.Contains(out, "next run") {
		t.Fatalf("enable output: %q", out)
	}
	got, _ = st.Get(a.ID)
	if !got.Enabled || got.NextRunAt == nil {
		t.Fatal("enable did not recompute next_run_at")
	}
}

func TestCLIRunsJSON(t *testing.T) {
	st, cursor := newTestStore(t)
	a := Automation{Title: "Nightly", Prompt: "p", WorkingDir: "/tmp",
		Schedule: Schedule{Kind: KindDaily, Time: "09:00"}, Enabled: true}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	runID, err := st.RecordFire(a.ID, cursor.t)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.RecordFinish(runID, RunFailed, "boom", "sess-1")

	out := runCLICapture(t, st, "runs", a.ID, "--json")
	var decoded struct {
		Runs []Run `json:"runs"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("runs --json not machine-readable: %v\n%s", err, out)
	}
	if len(decoded.Runs) != 1 || decoded.Runs[0].Status != RunFailed || decoded.Runs[0].SessionID != "sess-1" {
		t.Fatalf("runs --json drifted: %+v", decoded.Runs)
	}
}

func TestCLIDeleteRequiresYes(t *testing.T) {
	st, _ := newTestStore(t)
	a := Automation{Title: "t", Prompt: "p", WorkingDir: "/tmp",
		Schedule: Schedule{Kind: KindDaily, Time: "09:00"}, Enabled: true}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := runCLI([]string{"delete", a.ID}, st, &out); err == nil {
		t.Fatal("delete without --yes must refuse")
	}
	if _, ok := st.Get(a.ID); !ok {
		t.Fatal("refused delete removed the row")
	}
	runCLICapture(t, st, "delete", a.ID, "--yes")
	if _, ok := st.Get(a.ID); ok {
		t.Fatal("delete --yes did not remove the row")
	}
}

func TestCLIUpdatePatchSemantics(t *testing.T) {
	cases := []struct {
		name string
		args []string
		want func(t *testing.T, got Automation)
	}{
		{
			name: "prompt only",
			args: []string{"update", "PLACEHOLDER", "--prompt", "fresh prompt"},
			want: func(t *testing.T, got Automation) {
				if got.Prompt != "fresh prompt" || got.Title != "Nightly" {
					t.Fatalf("patch drifted: %+v", got)
				}
			},
		},
		{
			name: "title and dir",
			args: []string{"update", "PLACEHOLDER", "--title", "Renamed", "--dir", "/elsewhere"},
			want: func(t *testing.T, got Automation) {
				// The store absolutizes the working dir: "/elsewhere" stays
				// itself on POSIX but becomes the drive-rooted path on Windows.
				wantDir, err := filepath.Abs("/elsewhere")
				if err != nil {
					t.Fatal(err)
				}
				if got.Title != "Renamed" || got.WorkingDir != wantDir {
					t.Fatalf("patch drifted: %+v", got)
				}
			},
		},
		{
			name: "bare time modifier re-times in place",
			args: []string{"update", "PLACEHOLDER", "--time", "23:45"},
			want: func(t *testing.T, got Automation) {
				if got.Schedule.Kind != KindDaily || got.Schedule.Time != "23:45" {
					t.Fatalf("schedule patch drifted: %+v", got.Schedule)
				}
			},
		},
		{
			name: "bare timezone modifier",
			args: []string{"update", "PLACEHOLDER", "--timezone", "Europe/Berlin"},
			want: func(t *testing.T, got Automation) {
				if got.Schedule.Timezone != "Europe/Berlin" || got.Schedule.Time != "09:00" {
					t.Fatalf("timezone patch drifted: %+v", got.Schedule)
				}
			},
		},
		{
			name: "weekly selector replaces the schedule",
			args: []string{"update", "PLACEHOLDER", "--weekly", "1,5", "--time", "08:00"},
			want: func(t *testing.T, got Automation) {
				if got.Schedule.Kind != KindWeekly || len(got.Schedule.Weekdays) != 2 || got.Schedule.Time != "08:00" {
					t.Fatalf("selector replace drifted: %+v", got.Schedule)
				}
			},
		},
		{
			name: "hourly with minute",
			args: []string{"update", "PLACEHOLDER", "--hourly", "--minute", "40"},
			want: func(t *testing.T, got Automation) {
				if got.Schedule.Kind != KindHourly || got.Schedule.Minute != 40 {
					t.Fatalf("selector replace drifted: %+v", got.Schedule)
				}
			},
		},
		{
			name: "disable via update",
			args: []string{"update", "PLACEHOLDER", "--disable"},
			want: func(t *testing.T, got Automation) {
				if got.Enabled {
					t.Fatal("--disable not applied")
				}
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newTestStore(t)
			a := Automation{Title: "Nightly", Prompt: "p", WorkingDir: "/tmp",
				Schedule: Schedule{Kind: KindDaily, Time: "09:00"}, Enabled: true}
			if err := st.Create(&a); err != nil {
				t.Fatal(err)
			}
			args := append([]string{}, tc.args...)
			args[1] = a.ID
			runCLICapture(t, st, args...)
			got, ok := st.Get(a.ID)
			if !ok {
				t.Fatal("update lost the record")
			}
			tc.want(t, got)
		})
	}
}

func TestCLIUpdateJSONAndNextRun(t *testing.T) {
	st, cursor := newTestStore(t)
	a := Automation{Title: "Nightly", Prompt: "p", WorkingDir: "/tmp",
		Schedule: Schedule{Kind: KindDaily, Time: "09:00"}, Enabled: true}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	// A schedule change re-times the next run from the store clock.
	out := runCLICapture(t, st, "update", a.ID, "--time", "18:30", "--json")
	var decoded struct {
		Automation Automation `json:"automation"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("update --json not machine-readable: %v\n%s", err, out)
	}
	got := decoded.Automation
	if got.Schedule.Time != "18:30" {
		t.Fatalf("update --json drifted: %+v", got)
	}
	want := time.Date(2026, 1, 5, 18, 30, 0, 0, time.Local) // empty timezone = local
	if got.NextRunAt == nil || !got.NextRunAt.Equal(want) {
		t.Fatalf("re-timed next run = %v, want %v", got.NextRunAt, want)
	}
	_ = cursor
}

func TestCLIUpdateRejections(t *testing.T) {
	cases := []struct {
		name string
		args []string
	}{
		{"no fields", []string{"update", "auto-x"}},
		{"two selectors", []string{"update", "auto-x", "--daily", "--hourly"}},
		{"enable and disable", []string{"update", "auto-x", "--enable", "--disable"}},
		{"bad weekly value", []string{"update", "auto-x", "--weekly", "9"}},
		{"bad timezone", []string{"update", "auto-x", "--timezone", "Mars/Base"}},
		{"unknown id", []string{"update", "auto-nope", "--title", "x"}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			st, _ := newTestStore(t)
			a := Automation{Title: "Nightly", Prompt: "p", WorkingDir: "/tmp",
				Schedule: Schedule{Kind: KindDaily, Time: "09:00"}, Enabled: true}
			if err := st.Create(&a); err != nil {
				t.Fatal(err)
			}
			var out bytes.Buffer
			args := append([]string{}, tc.args...)
			if args[len(args)-2] == "auto-x" {
				// Point the mutation cases at the real id; "auto-nope" stays.
				for i, arg := range args {
					if arg == "auto-x" {
						args[i] = a.ID
					}
				}
			}
			if err := runCLI(args, st, &out); err == nil {
				t.Fatalf("update accepted invalid input: %v", args)
			}
			got, _ := st.Get(a.ID)
			if got.Title != "Nightly" || got.Prompt != "p" || got.Schedule.Time != "09:00" {
				t.Fatalf("rejected update modified the record: %+v", got)
			}
		})
	}
}

func TestCLIUnknownCommandAndMissingID(t *testing.T) {
	st, _ := newTestStore(t)
	cases := [][]string{
		{},
		{"nonsense"},
		{"get"},
		{"enable"},
		{"runs"},
	}
	for _, args := range cases {
		var out bytes.Buffer
		if err := runCLI(args, st, &out); err == nil {
			t.Fatalf("runCLI(%v) should fail", args)
		}
	}
}

func TestCLICreateJSONOutput(t *testing.T) {
	st, _ := newTestStore(t)
	out := runCLICapture(t, st, "create", "JSON job", "--prompt", "p", "--dir", "/tmp", "--daily", "--time", "09:00", "--json")
	var decoded struct {
		Automation Automation `json:"automation"`
	}
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("create --json not machine-readable: %v\n%s", err, out)
	}
	if decoded.Automation.ID == "" || decoded.Automation.Schedule.Kind != KindDaily {
		t.Fatalf("create --json record incomplete: %+v", decoded.Automation)
	}
}

// TestCLIUpdateWeekdaysKeepsTimezone pins the patch path: replacing the
// schedule with a selector and no --timezone must keep the STORED zone.
// Dropping it re-based the automation on the host's local zone, so a
// "weekdays 09:00 Asia/Tokyo" row started firing at 09:00 wherever the host
// happened to be.
func TestCLIUpdateWeekdaysKeepsTimezone(t *testing.T) {
	st, _ := newTestStore(t)
	runCLICapture(t, st, "create", "My job", "--prompt", "do it", "--dir", "/tmp/proj",
		"--weekdays", "--time", "09:00", "--timezone", "Asia/Tokyo")
	autos := st.List()
	if len(autos) != 1 {
		t.Fatalf("create stored %d automations, want 1", len(autos))
	}
	id := autos[0].ID

	runCLICapture(t, st, "update", id, "--weekdays")

	updated, ok := st.Get(id)
	if !ok {
		t.Fatal("automation vanished after update")
	}
	if updated.Schedule.Timezone != "Asia/Tokyo" {
		t.Fatalf("timezone = %q after --weekdays update, want the stored Asia/Tokyo", updated.Schedule.Timezone)
	}
	if updated.Schedule.Kind != KindWeekdays {
		t.Fatalf("kind = %s, want weekdays", updated.Schedule.Kind)
	}
}

// TestParseClockRejectsTrailingGarbage pins the strict HH:MM parse: the old
// fmt.Sscanf("%d:%d") stopped after the second number and IGNORED the rest, so
// "9:30pm" silently became 09:30.
func TestParseClockRejectsTrailingGarbage(t *testing.T) {
	if h, m, err := ParseClock("09:30"); err != nil || h != 9 || m != 30 {
		t.Fatalf("ParseClock(09:30) = (%d,%d,%v)", h, m, err)
	}
	if h, m, err := ParseClock("9:30"); err != nil || h != 9 || m != 30 {
		t.Fatalf("ParseClock(9:30) = (%d,%d,%v)", h, m, err)
	}
	for _, bad := range []string{"9:30pm", "09:30:00", "9h30", "0930", ":", "09:", ":30", ""} {
		if bad == "" {
			continue // empty means the default, handled above
		}
		if _, _, err := ParseClock(bad); err == nil {
			t.Fatalf("ParseClock(%q) accepted, want an error", bad)
		}
	}
	if h, m, err := ParseClock(""); err != nil || h != 9 || m != 0 {
		t.Fatalf("ParseClock(empty) = (%d,%d,%v), want the 09:00 default", h, m, err)
	}
}
