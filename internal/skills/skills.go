// Package skills discovers structured, model-invocable skill instructions
// from project and user skill directories: bundle dirs (<name>/SKILL.md) or
// flat files (<name>.md). Skills are optional instructions, not session
// events: the agent lists them and reads a body into context on demand.
package skills

import (
	"cmp"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"gogen/internal/projectfile"
	"gopkg.in/yaml.v3"
)

const (
	// MaxSkillBodyBytes is the per-skill body cap; a larger skill file is
	// skipped entirely (never truncated).
	MaxSkillBodyBytes = 64 * 1024
	// MaxSkills caps the number of skills one discovery can return, so a
	// pathological skills directory cannot blow up the tool result.
	MaxSkills = 100
)

// kebabCase matches skill names: ^[a-z0-9]+(-[a-z0-9]+)*$.
var kebabCase = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9]+)*$`)

// Skill is one discovered skill with its full body.
type Skill struct {
	Name        string
	Description string // optional front-matter description
	Body        string // trimmed markdown body
	// Source names the root that supplied the skill ("project" or "user");
	// the project root wins a duplicate name outright.
	Source string
}

// Manager discovers skills on demand from the project and user roots.
// Stateless: every List/Read re-scans the directories AND re-resolves the
// project root from the current working directory, so a skill added,
// edited, or moved mid-session — or a working-dir change (/dir, web
// workspace change) — is visible immediately with no invalidation.
type Manager struct {
	// workingDir is re-read per call (projectRoot resolution), so the same
	// manager follows the agent's live working directory.
	workingDir string
	// globalMode skips the project root entirely (no .gogen directory).
	globalMode bool
}

// NewManager builds a skill manager for workingDir. In global mode only the
// user root is scanned (there is no project .gogen directory).
func NewManager(workingDir string, globalMode bool) *Manager {
	return &Manager{workingDir: workingDir, globalMode: globalMode}
}

// SetWorkingDir re-targets the project skills root after a working-dir
// change (/dir, web workspace change). The user root is unchanged. Mirrors
// BoardManager.SetWorkingDir so both feature managers follow the agent's
// live directory.
func (m *Manager) SetWorkingDir(dir string) {
	if m == nil {
		return
	}
	m.workingDir = dir
}

// userRoot returns the user skills directory (~/.config/gogen/skills).
func (m *Manager) userRoot() string {
	return filepath.Join(projectfile.GlobalConfigDir(), "skills")
}

// projectSkillsDir resolves the project skills root from the CURRENT
// working directory (empty in global mode).
func (m *Manager) projectSkillsDir() string {
	if m.globalMode {
		return ""
	}
	return filepath.Join(projectRoot(m.workingDir), ".gogen", "skills")
}

// projectRoot returns the nearest ancestor of workingDir containing a
// .git/.hg marker, falling back to workingDir itself.
func projectRoot(workingDir string) string {
	dir, err := filepath.Abs(workingDir)
	if err != nil {
		return workingDir
	}
	for {
		for _, marker := range []string{".git", ".hg"} {
			if _, err := os.Stat(filepath.Join(dir, marker)); err == nil {
				return dir
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return workingDir
		}
		dir = parent
	}
}

// List returns the discovered skills: project-root skills first (sorted by
// name), then user-root skills whose names were not already claimed by the
// project root. Capped at MaxSkills.
func (m *Manager) List() []Skill {
	if m == nil {
		return nil
	}
	var out []Skill
	seen := make(map[string]struct{})
	for _, root := range []struct {
		dir    string
		source string
	}{{m.projectSkillsDir(), "project"}, {m.userRoot(), "user"}} {
		if root.dir == "" {
			continue
		}
		for _, s := range scanRoot(root.dir) {
			if len(out) >= MaxSkills {
				return out
			}
			if _, dup := seen[s.Name]; dup {
				continue
			}
			seen[s.Name] = struct{}{}
			s.Source = root.source
			out = append(out, s)
		}
	}
	return out
}

// Read returns one skill by name, or an error when it does not exist.
//
// The candidate paths for name are resolved directly instead of scanning the
// roots with List (which reads and parses the body of every skill to return
// one), applying exactly the precedence List/scanRoot implement: the project
// root before the user root, and within one root a bundle
// (<root>/<name>/SKILL.md) before a flat <root>/<name>.md file. Read is not
// subject to the MaxSkills cap — that cap bounds a List result, not a
// single-skill lookup.
func (m *Manager) Read(name string) (Skill, error) {
	if !kebabCase.MatchString(name) {
		return Skill{}, fmt.Errorf("invalid skill name %q (want kebab-case)", name)
	}
	if m != nil {
		for _, root := range []struct {
			dir    string
			source string
		}{{m.projectSkillsDir(), "project"}, {m.userRoot(), "user"}} {
			if root.dir == "" {
				continue
			}
			if s, ok := readSkillAt(root.dir, name); ok {
				s.Source = root.source
				return s, nil
			}
		}
	}
	return Skill{}, fmt.Errorf("skill %q not found", name)
}

// readSkillAt reads the single skill name from one root, applying scanRoot's
// within-root precedence: a bundle (<root>/<name>/SKILL.md) wins over a flat
// file (<root>/<name>.md) of the same name, and the flat file is only
// consulted when the bundle is absent or unusable (unreadable, oversized, or
// without a body) — the same fall-through scanRoot's name dedupe produces.
// The returned Skill carries exactly the fields scanRoot builds (Source is
// the caller's to set).
func readSkillAt(root, name string) (Skill, bool) {
	if body, description, ok := readSkillFile(filepath.Join(root, name, "SKILL.md")); ok {
		return Skill{Name: name, Description: description, Body: body}, true
	}
	if body, description, ok := readSkillFile(filepath.Join(root, name+".md")); ok {
		return Skill{Name: name, Description: description, Body: body}, true
	}
	return Skill{}, false
}

// scanRoot scans one skill root: bundle dirs first (a bundle wins a flat
// file of the same name), then flat <name>.md files. Invalid names, hidden
// entries, and oversized bodies are skipped.
func scanRoot(root string) []Skill {
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil
	}
	var out []Skill
	seen := make(map[string]struct{})
	// Bundles: <name>/SKILL.md
	for _, e := range entries {
		if !e.IsDir() || strings.HasPrefix(e.Name(), ".") || !kebabCase.MatchString(e.Name()) {
			continue
		}
		body, desc, ok := readSkillFile(filepath.Join(root, e.Name(), "SKILL.md"))
		if !ok {
			continue
		}
		seen[e.Name()] = struct{}{}
		out = append(out, Skill{Name: e.Name(), Description: desc, Body: body})
	}
	// Flat files: <name>.md (skipped when a bundle claimed the name)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".md") {
			continue
		}
		name := strings.TrimSuffix(e.Name(), ".md")
		if strings.HasPrefix(name, ".") || !kebabCase.MatchString(name) {
			continue
		}
		if _, claimed := seen[name]; claimed {
			continue
		}
		body, desc, ok := readSkillFile(filepath.Join(root, e.Name()))
		if !ok {
			continue
		}
		out = append(out, Skill{Name: name, Description: desc, Body: body})
	}
	slices.SortFunc(out, func(a, b Skill) int { return cmp.Compare(a.Name, b.Name) })
	return out
}

// readSkillFile reads and validates one skill file: size cap, optional YAML
// front matter (description only), and a non-empty trimmed body.
func readSkillFile(path string) (body, description string, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil || len(data) > MaxSkillBodyBytes {
		return "", "", false
	}
	content := string(data)
	if strings.HasPrefix(strings.TrimRight(content, "\n"), "---") {
		end := strings.Index(content, "\n---")
		if end < 0 {
			return "", "", false
		}
		var fm struct {
			Description string `yaml:"description"`
		}
		if err := yaml.Unmarshal([]byte(content[3:end]), &fm); err != nil {
			return "", "", false
		}
		description = strings.TrimSpace(fm.Description)
		content = content[end+4:]
	}
	body = strings.TrimSpace(content)
	if body == "" {
		return "", "", false
	}
	return body, description, true
}
