package automation

import (
	"path/filepath"
	"testing"
	"time"
)

// updateBase creates a stored daily automation to update from.
func updateBase(t *testing.T, st *Store) Automation {
	t.Helper()
	a := Automation{
		Title: "Nightly triage", Prompt: "triage the board", WorkingDir: "/tmp/proj",
		Schedule: Schedule{Kind: KindDaily, Time: "09:00", Timezone: "UTC"}, Enabled: true,
	}
	if err := st.Create(&a); err != nil {
		t.Fatal(err)
	}
	return a
}

func TestUpdateFields(t *testing.T) {
	cases := []struct {
		name        string
		mutate      func(*Automation)
		check       func(t *testing.T, got Automation)
		wantNextRun *time.Time // nil = don't care; zero = cleared
	}{
		{
			name: "prompt only keeps the stored next run",
			mutate: func(a *Automation) {
				a.Prompt = "updated prompt"
			},
			check: func(t *testing.T, got Automation) {
				if got.Prompt != "updated prompt" || got.Title != "Nightly triage" {
					t.Fatalf("fields drifted: %+v", got)
				}
			},
		},
		{
			name: "schedule change re-times next run",
			mutate: func(a *Automation) {
				a.Schedule = Schedule{Kind: KindDaily, Time: "23:30", Timezone: "UTC"}
			},
			check: func(t *testing.T, got Automation) {
				if got.Schedule.Time != "23:30" {
					t.Fatalf("schedule not applied: %+v", got.Schedule)
				}
			},
		},
		{
			name: "schedule change to once takes the stored instant",
			mutate: func(a *Automation) {
				a.Schedule = Schedule{Kind: KindOnce, At: "2030-06-01T12:00:00Z"}
			},
			check: func(t *testing.T, got Automation) {
				if got.Schedule.Kind != KindOnce {
					t.Fatalf("schedule kind not applied: %+v", got.Schedule)
				}
			},
		},
		{
			name: "pause keeps everything else",
			mutate: func(a *Automation) {
				a.Enabled = false
			},
			check: func(t *testing.T, got Automation) {
				if got.Enabled {
					t.Fatal("pause not applied")
				}
			},
		},
		{
			name: "pausing and re-enabling in one update re-times",
			mutate: func(a *Automation) {
				a.Enabled = true // already enabled: fresh-enable rule does not fire
			},
			check: func(t *testing.T, got Automation) {
				if !got.Enabled {
					t.Fatal("enable not applied")
				}
			},
		},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			st, cursor := newTestStore(t)
			a := updateBase(t, st)
			// Move next_run_at to a known value so the keep-vs-recompute
			// policy is observable.
			known := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC)
			setStoredNextRun(t, st, a.ID, known)

			updated := a
			tc.mutate(&updated)
			if err := st.Update(&updated); err != nil {
				t.Fatalf("Update: %v", err)
			}
			got, ok := st.Get(a.ID)
			if !ok {
				t.Fatal("update lost the record")
			}
			tc.check(t, got)
			if got.UpdatedAt.Before(got.CreatedAt) {
				t.Fatalf("UpdatedAt not advanced: %+v", got)
			}
			// Id and history are preserved.
			if got.ID != a.ID {
				t.Fatalf("id changed: %s -> %s", a.ID, got.ID)
			}

			switch tc.name {
			case "prompt only keeps the stored next run":
				if got.NextRunAt == nil || !got.NextRunAt.Equal(known) {
					t.Fatalf("prompt edit must not re-time the run: got %v, want %v", got.NextRunAt, known)
				}
			case "schedule change re-times next run":
				want := time.Date(2026, 1, 5, 23, 30, 0, 0, time.UTC)
				if got.NextRunAt == nil || !got.NextRunAt.Equal(want) {
					t.Fatalf("re-timed next run = %v, want %v", got.NextRunAt, want)
				}
			case "schedule change to once takes the stored instant":
				want := time.Date(2030, 6, 1, 12, 0, 0, 0, time.UTC)
				if got.NextRunAt == nil || !got.NextRunAt.Equal(want) {
					t.Fatalf("once next run = %v, want the new At %v", got.NextRunAt, want)
				}
			case "pause keeps everything else":
				if got.NextRunAt == nil || !got.NextRunAt.Equal(known) {
					t.Fatalf("pause must keep the stored next run: got %v", got.NextRunAt)
				}
			case "pausing and re-enabling in one update re-times":
				if got.NextRunAt == nil || !got.NextRunAt.Equal(known) {
					t.Fatalf("already-enabled row keeps its next run: got %v, want %v", got.NextRunAt, known)
				}
			}
			_ = cursor
		})
	}
}

// TestUpdateEnableOnPausedRecomputes pins the fresh-enable rule of Update:
// a paused row updated with enabled=true (schedule unchanged) gets its next
// run recomputed from now, exactly like SetEnabled(true).
func TestUpdateEnableOnPausedRecomputes(t *testing.T) {
	st, cursor := newTestStore(t)
	a := updateBase(t, st)
	if err := st.SetEnabled(a.ID, false); err != nil {
		t.Fatal(err)
	}
	cursor.Advance(3 * time.Hour) // the pause lasted a while

	updated := a
	updated.Enabled = true
	if err := st.Update(&updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := st.Get(a.ID)
	want := time.Date(2026, 1, 6, 9, 0, 0, 0, time.UTC) // next 09:00 UTC after the advanced cursor (Jan 5, 12:00)
	if got.NextRunAt == nil || !got.NextRunAt.Equal(want) {
		t.Fatalf("re-enabled next run = %v, want %v", got.NextRunAt, want)
	}
}

// TestUpdateRelativeWorkingDirAbsolutized matches Create's normalization.
func TestUpdateRelativeWorkingDirAbsolutized(t *testing.T) {
	st, _ := newTestStore(t)
	a := updateBase(t, st)
	updated := a
	updated.WorkingDir = "relative/path"
	if err := st.Update(&updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	got, _ := st.Get(a.ID)
	if !absPath(got.WorkingDir) {
		t.Fatalf("working dir not absolutized: %q", got.WorkingDir)
	}
}

func absPath(p string) bool {
	return filepath.IsAbs(p)
}

// TestUpdatePersistsAcrossReopen covers the file round-trip.
func TestUpdatePersistsAcrossReopen(t *testing.T) {
	dir := t.TempDir()
	st, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	a := updateBase(t, st)
	updated := a
	updated.Title = "Renamed nightly"
	updated.Prompt = "new prompt"
	if err := st.Update(&updated); err != nil {
		t.Fatalf("Update: %v", err)
	}
	st2, err := OpenStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := st2.Get(a.ID)
	if !ok || got.Title != "Renamed nightly" || got.Prompt != "new prompt" {
		t.Fatalf("update did not persist: %+v (ok=%v)", got, ok)
	}
}

// TestUpdateUnknownIDAndValidation pins the error paths.
func TestUpdateUnknownIDAndValidation(t *testing.T) {
	st, _ := newTestStore(t)
	a := updateBase(t, st)

	ghost := a
	ghost.ID = "auto-nope"
	if err := st.Update(&ghost); err == nil {
		t.Fatal("Update on an unknown id should fail")
	}

	bad := a
	bad.Schedule = Schedule{Kind: "fortnightly"}
	if err := st.Update(&bad); err == nil {
		t.Fatal("Update with an invalid schedule should fail")
	}
	// The failed validation must not have touched the record.
	got, _ := st.Get(a.ID)
	if got.Schedule.Kind != KindDaily {
		t.Fatalf("rejected update modified the record: %+v", got)
	}
}

// TestUpdateKeepsNextRunThroughSweep pins the motivation for the policy: a
// prompt-only edit minutes before the scheduled time leaves the run where
// it was (the scheduler fires it at the original time).
func TestUpdateKeepsNextRunThroughSweep(t *testing.T) {
	st, cursor := newTestStore(t)
	a := updateBase(t, st)
	setStoredNextRun(t, st, a.ID, cursor.t.Add(2*time.Minute))

	updated := a
	updated.Prompt = "urgent fix: also check the changelog"
	if err := st.Update(&updated); err != nil {
		t.Fatalf("Update: %v", err)
	}

	// 2 minutes later the sweep claims the run — with the NEW prompt.
	cursor.Advance(2 * time.Minute)
	claims := st.ClaimDue(cursor.t, DefaultCatchupWindow)
	if len(claims) != 1 {
		t.Fatalf("claim count = %d, want 1", len(claims))
	}
	if claims[0].Automation.Prompt != updated.Prompt {
		t.Fatalf("fired prompt = %q, want the updated one", claims[0].Automation.Prompt)
	}
}
