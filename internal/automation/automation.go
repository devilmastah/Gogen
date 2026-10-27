// Package automation implements the file-based cron scheduler: saved
// prompt + working-dir + schedule records ("automations") that a running
// GoGen host (TUI or web server) fires as headless `-p` runs at the
// scheduled time, recording each run in a small history.
//
// Layout (no daemon):
//   - Automations live in the global config dir as JSON (automations.json);
//     the run history is a sibling JSON file (automation_runs.json). Every
//     write takes an advisory cross-process lock and re-reads the file first,
//     so the CLI and a running host never overwrite each other's edits.
//   - A 30 s in-process ticker claims due automations, advances their
//     next_run_at within that read-modify-write (the dedup guarantee — a
//     claimed row cannot fire twice, and two hosts sweeping at once claim an
//     occurrence exactly once between them), and fires each as a `gogen -p`
//     subprocess in the configured directory (see ProcessFire).
//   - Automations fire only while a GoGen host with `automations: on` is
//     running. Runs missed while no host was up are governed by the
//     catch-up policy documented on Scheduler (run once when overdue
//     within the window, otherwise record a missed run and advance).
package automation

import (
	"fmt"
	"strings"
	"time"

	"gogen/internal/randhex"
)

// Schedule kinds (the `kind` field of Schedule). Exactly one applies.
const (
	KindOnce     = "once"     // At: fire once at the stored instant
	KindHourly   = "hourly"   // Minute: every hour at :MM
	KindDaily    = "daily"    // Time: every day at HH:MM
	KindWeekdays = "weekdays" // Time: Monday–Friday at HH:MM
	KindWeekly   = "weekly"   // Weekdays + Time: selected weekdays at HH:MM
)

// Weekday convention: 0 = Sunday … 6 = Saturday (Go's time.Weekday values,
// also the JS getDay convention the reference design uses for its CLI).
//
// Run statuses (Run.Status / Automation.LastRunStatus):
//
//	running     — the headless run process was spawned and has not exited yet
//	completed   — the run process exited 0
//	failed      — the run could not be spawned, or exited non-zero / timed out
//	missed      — due while no host was running and beyond the catch-up window
//	interrupted — the host exited before the run finished (marked at startup)
const (
	RunRunning     = "running"
	RunCompleted   = "completed"
	RunFailed      = "failed"
	RunMissed      = "missed"
	RunInterrupted = "interrupted"
)

// Schedule describes when an automation fires. Kind selects which fields
// are meaningful (see the Kind* constants).
type Schedule struct {
	Kind string `json:"kind"`
	// At is the fire instant for kind=once (RFC 3339).
	At string `json:"at,omitempty"`
	// Time is the wall-clock fire time "HH:MM" for daily/weekdays/weekly.
	Time string `json:"time,omitempty"`
	// Minute is the minute-of-hour (0–59) for hourly.
	Minute int `json:"minute,omitempty"`
	// Weekdays lists the active weekdays (0 = Sunday … 6 = Saturday) for
	// weekly. Empty means the schedule never fires (rejected at create).
	Weekdays []int `json:"weekdays,omitempty"`
	// Timezone is an IANA zone name (e.g. "America/New_York") the
	// wall-clock times are evaluated in. Empty means the host's local zone.
	Timezone string `json:"timezone,omitempty"`
}

// Automation is one saved prompt + working dir + schedule. Enabled gates
// firing; NextRunAt is the claimed/advanced fire time the scheduler sweep
// reads; the Last* fields mirror the most recent run (see Run).
type Automation struct {
	ID         string     `json:"id"`
	Title      string     `json:"title"`
	Prompt     string     `json:"prompt"`
	WorkingDir string     `json:"working_dir"`
	Schedule   Schedule   `json:"schedule"`
	Enabled    bool       `json:"enabled"`
	NextRunAt  *time.Time `json:"next_run_at,omitempty"`
	LastRunAt  *time.Time `json:"last_run_at,omitempty"`
	// LastRunStatus is the status of the most recent run ("" = never run).
	LastRunStatus string    `json:"last_run_status,omitempty"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// Run is one history entry: the scheduled time (PlannedAt), when the run
// actually fired, its outcome, and the headless session it produced.
type Run struct {
	ID           string     `json:"id"`
	AutomationID string     `json:"automation_id"`
	PlannedAt    time.Time  `json:"planned_at"`
	FiredAt      *time.Time `json:"fired_at,omitempty"`
	EndedAt      *time.Time `json:"ended_at,omitempty"`
	Status       string     `json:"status"`
	Detail       string     `json:"detail,omitempty"`
	// SessionID is the id of the headless session the run started
	// ("" when the run failed before a session was created).
	SessionID string `json:"session_id,omitempty"`
	// Host is the identity of the host process that started the run (the
	// machine's hostname, written when the run is recorded as running) and PID
	// is that process's OS pid. Together they let a starting host leave a run
	// alone that another LIVE host is executing. Both are empty/0 on rows
	// written by binaries that predate host identity; such a row is treated as
	// unowned (see Store.MarkInterruptedRuns).
	Host string `json:"host,omitempty"`
	PID  int    `json:"pid,omitempty"`
}

// NewID returns a fresh automation id ("auto-<hex>").
func NewID() string {
	return randhex.ID(4, "auto-")
}

// NewRunID returns a fresh run-history id ("run-<hex>").
func NewRunID() string {
	return randhex.ID(4, "run-")
}

// Validate checks the automation's user-supplied fields. Schedule problems
// are reported here (bad kind, malformed time, invalid timezone, empty
// weekly weekday list) so a broken record can never be stored; the store
// and the CLI both call it before persisting.
func (a *Automation) Validate() error {
	if strings.TrimSpace(a.Title) == "" {
		return fmt.Errorf("title must not be empty")
	}
	if strings.TrimSpace(a.Prompt) == "" {
		return fmt.Errorf("prompt must not be empty")
	}
	if strings.TrimSpace(a.WorkingDir) == "" {
		return fmt.Errorf("working dir must not be empty")
	}
	return validateSchedule(a.Schedule)
}

// validateSchedule checks one schedule's kind-specific fields.
func validateSchedule(s Schedule) error {
	switch s.Kind {
	case KindOnce:
		if _, err := ParseAt(s.At, s.Timezone); err != nil {
			return fmt.Errorf("at: %w", err)
		}
	case KindHourly:
		if s.Minute < 0 || s.Minute > 59 {
			return fmt.Errorf("minute must be 0–59")
		}
	case KindDaily, KindWeekdays, KindWeekly:
		if _, _, err := ParseClock(s.Time); err != nil {
			return fmt.Errorf("time: %w", err)
		}
		if s.Kind != KindWeekly {
			break
		}
		if err := validateWeekdays(s.Weekdays); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown schedule kind %q (want once, hourly, daily, weekdays, or weekly)", s.Kind)
	}
	if s.Timezone != "" {
		if _, err := LoadLocation(s.Timezone); err != nil {
			return fmt.Errorf("timezone: %w", err)
		}
	}
	return nil
}

// validateWeekdays checks the weekly schedule's weekday list.
func validateWeekdays(days []int) error {
	if len(days) == 0 {
		return fmt.Errorf("weekly schedule needs at least one weekday (0 = Sunday … 6 = Saturday)")
	}
	for _, d := range days {
		if d < 0 || d > 6 {
			return fmt.Errorf("weekday %d out of range (0 = Sunday … 6 = Saturday)", d)
		}
	}
	return nil
}

// Describe renders the schedule for humans ("daily 09:00 Europe/Berlin"),
// used by the CLI table and error messages.
func (s Schedule) Describe() string {
	loc := s.Timezone
	if loc == "" {
		loc = "local"
	}
	switch s.Kind {
	case KindOnce:
		return "once " + s.At
	case KindHourly:
		return fmt.Sprintf("hourly :%02d %s", s.Minute, loc)
	case KindDaily:
		return fmt.Sprintf("daily %s %s", s.Time, loc)
	case KindWeekdays:
		return fmt.Sprintf("weekdays %s %s", s.Time, loc)
	case KindWeekly:
		return fmt.Sprintf("weekly %s %s %s", formatWeekdays(s.Weekdays), s.Time, loc)
	default:
		return s.Kind + " (?)"
	}
}

func formatWeekdays(days []int) string {
	names := [...]string{"Sun", "Mon", "Tue", "Wed", "Thu", "Fri", "Sat"}
	parts := make([]string, len(days))
	for i, d := range days {
		if d < 0 || d > 6 {
			parts[i] = "?"
			continue
		}
		parts[i] = names[d]
	}
	return strings.Join(parts, ",")
}

// clone returns a deep copy (the *time.Time fields must not alias between
// the store's map and the values it hands out).
func (a *Automation) clone() Automation {
	out := *a
	out.NextRunAt = cloneTime(a.NextRunAt)
	out.LastRunAt = cloneTime(a.LastRunAt)
	out.Schedule.Weekdays = append([]int(nil), a.Schedule.Weekdays...)
	return out
}

func cloneTime(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	out := *t
	return &out
}
