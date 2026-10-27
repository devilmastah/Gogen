package automation

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"gogen/internal/onoff"
)

// RunCLI executes the `gogen automation` subcommand tree:
//
//	gogen automation ls [--json]
//	gogen automation get <id> [--json]
//	gogen automation create <title> --prompt P [--dir PATH]
//	    (--at INSTANT | --daily | --hourly | --weekdays | --weekly DAYS)
//	    [--time HH:MM] [--minute M] [--timezone IANA] [--disabled] [--json]
//	gogen automation enable <id> | disable <id>
//	gogen automation runs <id> [--json]
//	gogen automation delete <id> [--yes]
//
// The commands talk straight to the file store — no agent, no API key — so
// they also work while no GoGen host is running (scheduling itself needs a
// running host, see the package docs). `--json` switches every command to
// machine-readable output.
func RunCLI(args []string) error {
	return runCLI(args, nil, os.Stdout)
}

func runCLI(args []string, store *Store, stdout io.Writer) error {
	if len(args) == 0 {
		return usageError{msg: "missing subcommand (want ls, get, create, enable, disable, runs, delete)"}
	}
	cmd, rest := args[0], args[1:]
	switch cmd {
	case "ls", "list":
		return cliList(rest, store, stdout)
	case "get":
		return cliGet(rest, store, stdout)
	case "create":
		return cliCreate(rest, store, stdout)
	case "update", "edit":
		return cliUpdate(rest, store, stdout)
	case "enable", "disable":
		return cliSetEnabled(cmd == "enable", rest, store, stdout)
	case "runs":
		return cliRuns(rest, store, stdout)
	case "delete", "rm":
		return cliDelete(rest, store, stdout)
	case "-h", "--help", "help":
		return usageError{msg: cliUsage()}
	default:
		return usageError{msg: fmt.Sprintf("unknown automation subcommand %q (want ls, get, create, update, enable, disable, runs, delete)", cmd)}
	}
}

// reorderArgs moves flags (and their values) ahead of the positional
// arguments, so the standard flag package accepts the natural
// `gogen automation get <id> --json` order (flag.Parse stops at the first
// non-flag token). valueFlags lists the flags that consume the next token
// (e.g. --time 09:00); everything else is boolean.
func reorderArgs(args []string, valueFlags map[string]bool) []string {
	var flags, positional []string
	for i := 0; i < len(args); i++ {
		tok := args[i]
		if tok == "--" {
			positional = append(positional, args[i+1:]...)
			break
		}
		if len(tok) > 1 && tok[0] == '-' {
			flags = append(flags, tok)
			name := strings.TrimLeft(tok, "-")
			if !strings.Contains(name, "=") && valueFlags[name] && i+1 < len(args) {
				i++
				flags = append(flags, args[i])
			}
			continue
		}
		positional = append(positional, tok)
	}
	return append(flags, positional...)
}

// createValueFlags are the `create` flags that consume a following value.
var createValueFlags = map[string]bool{
	"prompt": true, "dir": true, "at": true, "weekly": true,
	"time": true, "minute": true, "timezone": true,
}

// updateValueFlags are the `update` flags that consume a following value.
var updateValueFlags = map[string]bool{
	"title": true, "prompt": true, "dir": true, "at": true, "weekly": true,
	"time": true, "minute": true, "timezone": true,
}

// noValueFlags: the other subcommands only carry boolean flags.
var noValueFlags = map[string]bool{}

// usageError marks a user-input error whose message is the help text.
type usageError struct{ msg string }

func (e usageError) Error() string { return e.msg }

// openStore resolves the store for the CLI: the injected instance (tests)
// or the default global store.
func openStore(store *Store) (*Store, error) {
	if store != nil {
		return store, nil
	}
	return OpenStore("")
}

func cliList(args []string, store *Store, stdout io.Writer) error {
	fs := flag.NewFlagSet("ls", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, noValueFlags)); err != nil {
		return err
	}
	st, err := openStore(store)
	if err != nil {
		return err
	}
	autos := st.List()
	if *asJSON {
		return json.NewEncoder(stdout).Encode(map[string]any{"automations": autos})
	}
	if len(autos) == 0 {
		fmt.Fprintln(stdout, "No automations. Create one with: gogen automation create <title> --prompt ... --daily --time 09:00")
		return nil
	}
	fmt.Fprintf(stdout, "%-18s %-3s %-28s %-34s %-20s %s\n", "ID", "ON", "TITLE", "SCHEDULE", "NEXT RUN", "LAST")
	for _, a := range autos {
		fmt.Fprintf(stdout, "%-18s %-3s %-28s %-34s %-20s %s\n",
			a.ID, onOff(a.Enabled), clip(a.Title, 28), a.Schedule.Describe(),
			formatTime(a.NextRunAt), lastRunSummary(a))
	}
	return nil
}

func cliGet(args []string, store *Store, stdout io.Writer) error {
	fs := flag.NewFlagSet("get", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, noValueFlags)); err != nil {
		return err
	}
	id, err := requireID(fs.Args())
	if err != nil {
		return err
	}
	st, err := openStore(store)
	if err != nil {
		return err
	}
	a, ok := st.Get(id)
	if !ok {
		return fmt.Errorf("no automation with id %s", id)
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(map[string]any{"automation": a})
	}
	writeAutomation(stdout, a)
	return nil
}

func cliCreate(args []string, store *Store, stdout io.Writer) error {
	fs := flag.NewFlagSet("create", flag.ContinueOnError)
	prompt := fs.String("prompt", "", "prompt to run (required)")
	dir := fs.String("dir", "", "working directory for the run (default: current dir)")
	sched := registerScheduleFlags(fs)
	disabled := fs.Bool("disabled", false, "create paused (enabled=false)")
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, createValueFlags)); err != nil {
		return err
	}
	sched.detect(fs)
	title := strings.Join(fs.Args(), " ")
	if title == "" {
		return usageError{msg: "usage: gogen automation create <title> [flags]"}
	}

	schedule, err := sched.build(*sched.timezone)
	if err != nil {
		return err
	}
	workDir := *dir
	if workDir == "" {
		workDir = "."
	}
	a := Automation{
		Title:      title,
		Prompt:     *prompt,
		WorkingDir: workDir,
		Schedule:   schedule,
		Enabled:    !*disabled,
	}
	if err := a.Validate(); err != nil {
		return err
	}
	st, err := openStore(store)
	if err != nil {
		return err
	}
	if err := st.Create(&a); err != nil {
		return err
	}
	if *asJSON {
		return json.NewEncoder(stdout).Encode(map[string]any{"automation": a})
	}
	fmt.Fprintf(stdout, "Created %s %q\n", a.ID, a.Title)
	writeAutomation(stdout, a)
	return nil
}

func cliUsage() string {
	return "usage: gogen automation ls | get <id> | create <title> [schedule] | update <id> [fields] | enable <id> | disable <id> | runs <id> | delete <id>"
}

// scheduleFlags is the schedule half of `create` and `update`: the same flag
// names, defaults and help text in both, plus ONE answer to "which of them did
// the user actually set?".
//
// That single answer is the point. Create and update used to keep two separate
// counters and two sets of sentinels (an empty --time, a -1 --minute meaning
// "keep the stored value"), and their disagreement about what "unset" meant is
// what silently dropped a stored timezone on `update --weekdays` (B30).
// fs.Visit reports only the flags present on the command line, so set-vs-default
// is decided once, by the flag package, and both commands read the same map.
type scheduleFlags struct {
	at       *string
	weekly   *string
	time     *string
	minute   *int
	timezone *string
	// The boolean selectors are only meaningful through set: a false default
	// and "not passed" are otherwise indistinguishable. The pointers exist
	// because that is how the flag package registers them.
	daily    *bool
	hourly   *bool
	weekdays *bool
	// set holds the names of the flags the user really passed (see detect).
	set map[string]bool
}

// scheduleSelectors are the mutually exclusive schedule kinds.
var scheduleSelectors = [...]string{"at", "daily", "hourly", "weekdays", "weekly"}

// registerScheduleFlags adds the schedule flags to fs and returns their
// accessors. Call detect after fs.Parse.
func registerScheduleFlags(fs *flag.FlagSet) *scheduleFlags {
	return &scheduleFlags{
		at:       fs.String("at", "", "run once at this instant (RFC 3339, e.g. 2026-08-10T09:00:00Z)"),
		daily:    fs.Bool("daily", false, "every day at --time"),
		hourly:   fs.Bool("hourly", false, "every hour at :--minute"),
		weekdays: fs.Bool("weekdays", false, "Monday–Friday at --time"),
		weekly:   fs.String("weekly", "", "weekly on these weekdays, 0=Sun…6=Sat (e.g. 1,3,5)"),
		time:     fs.String("time", "09:00", "fire time HH:MM (daily/weekdays/weekly)"),
		minute:   fs.Int("minute", 0, "minute of hour (hourly)"),
		timezone: fs.String("timezone", "", "IANA timezone for wall-clock times (default: local)"),
	}
}

// detect records which schedule flags the user passed. It must run after
// fs.Parse: the set map is the ONLY source of truth for "did the user set
// this?", so no caller has to re-infer it from a value it cannot tell apart
// from the default.
func (f *scheduleFlags) detect(fs *flag.FlagSet) {
	f.set = map[string]bool{}
	fs.Visit(func(fl *flag.Flag) { f.set[fl.Name] = true })
}

// selectors counts the schedule kinds the user selected.
func (f *scheduleFlags) selectors() int {
	n := 0
	for _, name := range scheduleSelectors {
		if f.set[name] {
			n++
		}
	}
	return n
}

// build assembles the Schedule for the selected kind, with the flag defaults
// for the fields the user did not pass (create's rule: --time defaults to
// 09:00, --minute to 0). timezone is passed in because `create` uses the flag
// and `update` keeps the stored zone when the flag is absent.
func (f *scheduleFlags) build(timezone string) (Schedule, error) {
	if f.selectors() != 1 {
		return Schedule{}, usageError{msg: "pass exactly one schedule: --at | --daily | --hourly | --weekdays | --weekly <days>"}
	}
	timeStr := *f.time
	minute := *f.minute
	switch {
	case f.set["at"]:
		return Schedule{Kind: KindOnce, At: *f.at, Timezone: timezone}, nil
	case f.set["daily"]:
		return Schedule{Kind: KindDaily, Time: timeStr, Timezone: timezone}, nil
	case f.set["hourly"]:
		return Schedule{Kind: KindHourly, Minute: minute, Timezone: timezone}, nil
	case f.set["weekdays"]:
		return Schedule{Kind: KindWeekdays, Time: timeStr, Timezone: timezone}, nil
	default:
		days, err := ParseWeekdays(*f.weekly)
		if err != nil {
			return Schedule{}, err
		}
		return Schedule{Kind: KindWeekly, Weekdays: days, Time: timeStr, Timezone: timezone}, nil
	}
}

// applyPatch applies the schedule flags the user actually set to a stored
// record and reports whether anything schedule-related was touched (which is
// what re-times the next run — see Store.Update, which only re-times when the
// schedule really changed).
//
// A selector replaces the whole schedule with create's defaults; the bare
// modifiers (--time/--minute/--timezone) patch the stored schedule in place.
// Either way every field the user did NOT pass keeps its stored value — the
// timezone in the selector branch above all: dropping it re-based the
// automation on the host's local zone, so a "weekdays 09:00 in Asia/Tokyo" row
// started firing at 09:00 wherever the host happened to be (B30).
func (f *scheduleFlags) applyPatch(a *Automation) (bool, error) {
	switch n := f.selectors(); {
	case n > 1:
		return false, usageError{msg: "pass only one schedule selector: --at | --daily | --hourly | --weekdays | --weekly <days>"}
	case n == 1:
		timezone := *f.timezone
		if !f.set["timezone"] {
			timezone = a.Schedule.Timezone
		}
		schedule, err := f.build(timezone)
		if err != nil {
			return false, err
		}
		a.Schedule = schedule
		return true, nil
	}
	touched := false
	if f.set["time"] {
		a.Schedule.Time = *f.time
		touched = true
	}
	if f.set["timezone"] {
		a.Schedule.Timezone = *f.timezone
		touched = true
	}
	if f.set["minute"] {
		a.Schedule.Minute = *f.minute
		touched = true
	}
	return touched, nil
}

// cliUpdate is the PATCH-style edit: pass at least one field; omitted
// fields keep their stored values. A schedule selector (--at/--daily/
// --hourly/--weekdays/--weekly) replaces the whole schedule; the bare
// modifiers (--time/--minute/--timezone) patch the stored schedule's fields in
// place (see scheduleFlags.applyPatch). Either way a schedule change re-times
// next_run_at (prompt-only edits never do — see Store.Update).
func cliUpdate(args []string, store *Store, stdout io.Writer) error {
	fs := flag.NewFlagSet("update", flag.ContinueOnError)
	title := fs.String("title", "", "new title")
	prompt := fs.String("prompt", "", "new prompt")
	dir := fs.String("dir", "", "new working directory")
	sched := registerScheduleFlags(fs)
	enable := fs.Bool("enable", false, "enable the automation")
	disable := fs.Bool("disable", false, "pause the automation")
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, updateValueFlags)); err != nil {
		return err
	}
	sched.detect(fs)
	id, err := requireID(fs.Args())
	if err != nil {
		return err
	}
	if *enable && *disable {
		return usageError{msg: "pass only one of --enable / --disable"}
	}

	st, err := openStore(store)
	if err != nil {
		return err
	}
	stored, ok := st.Get(id)
	if !ok {
		return fmt.Errorf("no automation with id %s", id)
	}

	updated := stored
	scheduleTouched, err := sched.applyPatch(&updated)
	if err != nil {
		return err
	}
	// changed tracks "the user passed something": it is accumulated while the
	// fields are applied rather than re-derived from the flag values at the
	// end (re-deriving is where update and create drifted apart before).
	changed := scheduleTouched
	if *title != "" {
		updated.Title = *title
		changed = true
	}
	if *prompt != "" {
		updated.Prompt = *prompt
		changed = true
	}
	if *dir != "" {
		updated.WorkingDir = *dir
		changed = true
	}
	if *enable {
		updated.Enabled = true
		changed = true
	}
	if *disable {
		updated.Enabled = false
		changed = true
	}
	if !changed {
		return usageError{msg: "nothing to update — pass at least one field (--title/--prompt/--dir/--time/--minute/--timezone, a schedule selector, or --enable/--disable)"}
	}
	if err := st.Update(&updated); err != nil {
		return err
	}
	// Report the FRESH record: Update may have re-timed next_run_at.
	fresh, ok := st.Get(updated.ID)
	if !ok {
		return fmt.Errorf("no automation with id %s", id)
	}
	updated = fresh
	if *asJSON {
		return json.NewEncoder(stdout).Encode(map[string]any{"automation": updated})
	}
	fmt.Fprintf(stdout, "Updated %s %q\n", updated.ID, updated.Title)
	writeAutomation(stdout, updated)
	return nil
}

func cliSetEnabled(on bool, args []string, store *Store, stdout io.Writer) error {
	name := "disable"
	if on {
		name = "enable"
	}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	if err := fs.Parse(reorderArgs(args, noValueFlags)); err != nil {
		return err
	}
	id, err := requireID(fs.Args())
	if err != nil {
		return err
	}
	st, err := openStore(store)
	if err != nil {
		return err
	}
	if err := st.SetEnabled(id, on); err != nil {
		return err
	}
	a, ok := st.Get(id)
	if !ok {
		return fmt.Errorf("no automation with id %s", id)
	}
	if on {
		fmt.Fprintf(stdout, "Enabled %s %q; next run %s\n", a.ID, a.Title, formatTime(a.NextRunAt))
	} else {
		fmt.Fprintf(stdout, "Disabled %s %q (paused; re-enable with: gogen automation enable %s)\n", a.ID, a.Title, a.ID)
	}
	return nil
}

func cliRuns(args []string, store *Store, stdout io.Writer) error {
	fs := flag.NewFlagSet("runs", flag.ContinueOnError)
	asJSON := fs.Bool("json", false, "machine-readable JSON output")
	if err := fs.Parse(reorderArgs(args, noValueFlags)); err != nil {
		return err
	}
	id, err := requireID(fs.Args())
	if err != nil {
		return err
	}
	st, err := openStore(store)
	if err != nil {
		return err
	}
	if _, ok := st.Get(id); !ok {
		return fmt.Errorf("no automation with id %s", id)
	}
	runs := st.Runs(id)
	if *asJSON {
		return json.NewEncoder(stdout).Encode(map[string]any{"runs": runs})
	}
	if len(runs) == 0 {
		fmt.Fprintln(stdout, "No runs recorded yet.")
		return nil
	}
	fmt.Fprintf(stdout, "%-22s %-12s %-20s %s\n", "PLANNED", "STATUS", "SESSION", "DETAIL")
	for _, r := range runs {
		fmt.Fprintf(stdout, "%-22s %-12s %-20s %s\n",
			r.PlannedAt.Local().Format("2006-01-02 15:04"), r.Status, r.SessionID, clip(r.Detail, 60))
	}
	return nil
}

func cliDelete(args []string, store *Store, stdout io.Writer) error {
	fs := flag.NewFlagSet("delete", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "delete without confirmation")
	if err := fs.Parse(reorderArgs(args, noValueFlags)); err != nil {
		return err
	}
	id, err := requireID(fs.Args())
	if err != nil {
		return err
	}
	st, err := openStore(store)
	if err != nil {
		return err
	}
	a, ok := st.Get(id)
	if !ok {
		return fmt.Errorf("no automation with id %s", id)
	}
	if !*yes {
		return fmt.Errorf("refusing to delete %s %q without --yes", a.ID, a.Title)
	}
	if err := st.Delete(id); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "Deleted %s %q (and its run history)\n", a.ID, a.Title)
	return nil
}

// writeAutomation renders one automation in the human-readable layout.
func writeAutomation(w io.Writer, a Automation) {
	fmt.Fprintf(w, "id:          %s\n", a.ID)
	fmt.Fprintf(w, "title:       %s\n", a.Title)
	fmt.Fprintf(w, "schedule:    %s\n", a.Schedule.Describe())
	fmt.Fprintf(w, "working_dir: %s\n", a.WorkingDir)
	fmt.Fprintf(w, "enabled:     %s\n", onOff(a.Enabled))
	fmt.Fprintf(w, "next_run_at: %s\n", formatTime(a.NextRunAt))
	fmt.Fprintf(w, "last_run:    %s\n", lastRunSummary(a))
	fmt.Fprintf(w, "prompt:      %s\n", clip(a.Prompt, 200))
}

// requireID validates the positional automation-id argument.
func requireID(args []string) (string, error) {
	if len(args) == 0 {
		return "", usageError{msg: "missing automation id (copy it from `gogen automation ls`)"}
	}
	if len(args) > 1 {
		return "", usageError{msg: "too many arguments (want one automation id)"}
	}
	return args[0], nil
}

func onOff(v bool) string {
	return onoff.Format(v)
}

func clip(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", " ")
	runes := []rune(s)
	if len(runes) <= n {
		return s
	}
	return string(runes[:n-1]) + "…"
}

func formatTime(t *time.Time) string {
	if t == nil {
		return "-"
	}
	return t.Local().Format("2006-01-02 15:04")
}

func lastRunSummary(a Automation) string {
	if a.LastRunAt == nil || a.LastRunStatus == "" {
		return "-"
	}
	return a.LastRunStatus + " " + formatTime(a.LastRunAt)
}
