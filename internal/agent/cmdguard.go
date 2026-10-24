package agent

import (
	"fmt"
	"regexp"
	"strings"
)

// CommandBlockedError reports that a command matched one of the
// blockedCommandPatterns. It is distinct from the allowlist rejection so
// the executor can escalate a blocklist hit to an interactive user approval
// while leaving allowlist violations as hard blocks.
type CommandBlockedError struct {
	Pattern string // the regexp that matched
}

func (e *CommandBlockedError) Error() string {
	return fmt.Sprintf("command blocked by safety policy (matched %q)", e.Pattern)
}

// CommandGuard controls which shell commands the agent may run.
type CommandGuard struct {
	Mode      string   // blocklist, allowlist, off
	Allowlist []string // first token or full prefix match when Mode=allowlist
}

func NewCommandGuard(mode string, allowlist []string) *CommandGuard {
	mode = strings.ToLower(strings.TrimSpace(mode))
	switch mode {
	case "allowlist", "blocklist", "off":
	default:
		mode = "blocklist"
	}
	return &CommandGuard{Mode: mode, Allowlist: allowlist}
}

// blockedCommandPatterns targets destructive / privilege-escalation / remote-
// execution patterns. It intentionally does not block general shell execution
// helpers such as eval, bash -c, or sh -c: execute_command is meant to run
// arbitrary developer commands, and blocking those builtins without also
// blocking every equivalent (source, command substitution, scripts) would be
// inconsistent security theater. Use allowlist mode for tighter control.
//
// A match does not hard-block: the executor escalates it to an interactive
// user approval (see Executor.enforceCommandGuard), so the user can choose
// to allow a specific dangerous command. Unattended contexts (no approver on
// the context) still refuse it, fail-closed.
var blockedCommandPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bsudo\b`),
	regexp.MustCompile(`(?i)\bsu\s+-`),
	regexp.MustCompile(`(?i)\brm\s+(-[^\s]*\s+)*-[^\s]*r[^\s]*\s+(/|\./\.\./|/\*|~|\$HOME|\$\{HOME\}|\.)`),
	regexp.MustCompile(`(?i)\brm\s+(-[^\s]*\s+)*(/|\./\.\./|/\*|~|\$HOME|\$\{HOME\})`),
	regexp.MustCompile(`(?i)\brm\s+(-[^\s]*\s+)*--\s+(/|~|\$HOME|\$\{HOME\})`),
	regexp.MustCompile(`(?i)\bmkfs\b`),
	regexp.MustCompile(`(?i)\bdd\s+(if=|of=)`),
	regexp.MustCompile(`(?i)\bchmod\s+(-[^\s]*\s+)*777\b`),
	regexp.MustCompile(`(?i)\bchown\b[^\n]*/`),
	regexp.MustCompile(`(?i)(curl|wget)[^\n|]*\|\s*(ba)?sh\b`),
	regexp.MustCompile(`(?i)\|\s*(ba)?sh\s*(\s|$|<)`),
	regexp.MustCompile(`(?i)\|\s*(python3?|perl|ruby|node|zsh|php)\b`),
	regexp.MustCompile(`(?i)\b(python3?|perl|ruby|node)\s+-[ce]\b`),
	regexp.MustCompile(`(?i)\bfind\b[^\n]*\s-delete\b`),
	regexp.MustCompile(`(?i)\bshutdown\b`),
	regexp.MustCompile(`(?i)\breboot\b`),
	regexp.MustCompile(`(?i)\bpoweroff\b`),
	regexp.MustCompile(`(?i)\bkill\s+-9\s+1\b`),
	regexp.MustCompile(`(?i):\(\)\s*\{`), // fork bomb
	regexp.MustCompile(`(?i)\bnc\s+(-[^\s]*\s+)*-e\b`),
	regexp.MustCompile(`(?i)\b(nmap|masscan)\b`),
	regexp.MustCompile(`(?i)\bchmod\s+(-[^\s]*\s+)*\+[xs]\s+/`),
	regexp.MustCompile(`(?i)\btee\s+(/etc/|/usr/)`),
	regexp.MustCompile(`(?i)>\s*/etc/`),
	regexp.MustCompile(`(?i)\bxargs\s+(-[^\s]*\s+)*rm\b`),
}

func (g *CommandGuard) Check(command string) error {
	if g == nil || g.Mode == "off" {
		return nil
	}
	command = strings.TrimSpace(command)
	if command == "" {
		return fmt.Errorf("empty command")
	}

	switch g.Mode {
	case "allowlist":
		return g.checkAllowlist(command)
	default:
		return g.checkBlocklist(command)
	}
}

func (g *CommandGuard) checkAllowlist(command string) error {
	if len(g.Allowlist) == 0 {
		return fmt.Errorf("command blocked: allowlist mode is enabled but GOGEN_COMMAND_ALLOWLIST is empty")
	}
	if err := rejectShellMetacharacters(command); err != nil {
		return err
	}
	lower := strings.ToLower(command)
	for _, allowed := range g.Allowlist {
		allowed = strings.TrimSpace(strings.ToLower(allowed))
		if allowed == "" {
			continue
		}
		if lower == allowed || strings.HasPrefix(lower, allowed+" ") || strings.HasPrefix(lower, allowed+"\t") {
			return nil
		}
	}
	return fmt.Errorf("command blocked by allowlist: %q", command)
}

func rejectShellMetacharacters(command string) error {
	if strings.ContainsAny(command, ";|&`$()<>\n\r") {
		return fmt.Errorf("command blocked by allowlist: shell metacharacters are not permitted")
	}
	return nil
}

func (g *CommandGuard) checkBlocklist(command string) error {
	for _, re := range blockedCommandPatterns {
		if re.MatchString(command) {
			return &CommandBlockedError{Pattern: re.String()}
		}
	}
	return nil
}

func ParseAllowlist(raw string) []string {
	if strings.TrimSpace(raw) == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
