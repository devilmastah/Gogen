package skills

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// readFixture builds a manager over a temp project root and a temp user root
// (XDG_CONFIG_HOME), returning the manager and both skills directories.
func readFixture(t *testing.T) (m *Manager, projSkills, userSkills string) {
	t.Helper()
	_, proj := mkProject(t)
	xdg := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	return NewManager(proj, false),
		filepath.Join(proj, ".gogen", "skills"),
		filepath.Join(xdg, "gogen", "skills")
}

// TestReadPrecedence pins the resolution rules Read applies without scanning
// the roots: the project root before the user root, and within one root a
// bundle (<name>/SKILL.md) before a flat file (<name>.md).
func TestReadPrecedence(t *testing.T) {
	m, projSkills, userSkills := readFixture(t)
	// Project bundle, shadowed by nothing; the SAME name in the user root.
	writeSkill(t, filepath.Join(projSkills, "alpha", "SKILL.md"),
		"---\ndescription: Project alpha\n---\n# Alpha\n\nproject resume")
	writeSkill(t, filepath.Join(userSkills, "alpha", "SKILL.md"), "user alpha body")
	// Project flat file with front matter.
	writeSkill(t, filepath.Join(projSkills, "beta.md"), "---\ndescription: Flat beta\n---\nbeta body")
	// Present as BOTH bundle and flat in the project root: the bundle wins.
	writeSkill(t, filepath.Join(projSkills, "check", "SKILL.md"),
		"---\ndescription: Bundle check\n---\nbundle check body")
	writeSkill(t, filepath.Join(projSkills, "check.md"), "flat check body")
	// User-root only.
	writeSkill(t, filepath.Join(userSkills, "gamma.md"), "gamma body")

	tests := []struct {
		name string
		want Skill
	}{
		{
			name: "alpha",
			want: Skill{Name: "alpha", Description: "Project alpha", Body: "# Alpha\n\nproject resume", Source: "project"},
		},
		{
			name: "beta",
			want: Skill{Name: "beta", Description: "Flat beta", Body: "beta body", Source: "project"},
		},
		{
			name: "gamma",
			want: Skill{Name: "gamma", Body: "gamma body", Source: "user"},
		},
		{
			name: "check",
			want: Skill{Name: "check", Description: "Bundle check", Body: "bundle check body", Source: "project"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.Read(tt.name)
			if err != nil {
				t.Fatalf("Read(%q): %v", tt.name, err)
			}
			if got != tt.want {
				t.Fatalf("Read(%q) = %+v, want %+v", tt.name, got, tt.want)
			}
		})
	}
}

// TestReadErrors pins the error contracts: kebab-case validation, an unknown
// name, and oversized bodies (skipped whole — never returned half-read).
func TestReadErrors(t *testing.T) {
	m, projSkills, userSkills := readFixture(t)
	writeSkill(t, filepath.Join(projSkills, "ok.md"), "ok body")
	writeSkill(t, filepath.Join(projSkills, "huge.md"), strings.Repeat("a", MaxSkillBodyBytes+1))
	writeSkill(t, filepath.Join(userSkills, "user-huge.md"), strings.Repeat("b", MaxSkillBodyBytes+1))

	tests := []struct {
		name    string
		wantSub string
	}{
		{"missing", `skill "missing" not found`},
		{"huge", `skill "huge" not found`},
		{"user-huge", `skill "user-huge" not found`},
		{"Not_Kebab", `invalid skill name "Not_Kebab" (want kebab-case)`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := m.Read(tt.name)
			if err == nil {
				t.Fatalf("Read(%q) = %+v, want an error", tt.name, got)
			}
			if !strings.Contains(err.Error(), tt.wantSub) {
				t.Fatalf("Read(%q) error = %q, want it to contain %q", tt.name, err, tt.wantSub)
			}
		})
	}
}

// TestReadFallsBackFromUnusableBundle pins the within-root fall-through
// scanRoot's dedupe produces: an oversized bundle does not hide the flat file
// of the same name.
func TestReadFallsBackFromUnusableBundle(t *testing.T) {
	m, projSkills, _ := readFixture(t)
	writeSkill(t, filepath.Join(projSkills, "dup", "SKILL.md"), strings.Repeat("x", MaxSkillBodyBytes+1))
	writeSkill(t, filepath.Join(projSkills, "dup.md"), "flat dup body")

	got, err := m.Read("dup")
	if err != nil {
		t.Fatalf("Read(dup): %v", err)
	}
	want := Skill{Name: "dup", Body: "flat dup body", Source: "project"}
	if got != want {
		t.Fatalf("Read(dup) = %+v, want the flat file %+v", got, want)
	}
}

// TestReadAgreesWithList pins that Read resolves exactly what List reports —
// the drift guard for the direct-path lookup.
func TestReadAgreesWithList(t *testing.T) {
	m, projSkills, userSkills := readFixture(t)
	writeSkill(t, filepath.Join(projSkills, "alpha", "SKILL.md"), "---\ndescription: A\n---\nalpha body")
	writeSkill(t, filepath.Join(projSkills, "alpha.md"), "shadowed flat body")
	writeSkill(t, filepath.Join(projSkills, "beta.md"), "beta body")
	writeSkill(t, filepath.Join(userSkills, "alpha.md"), "shadowed user body")
	writeSkill(t, filepath.Join(userSkills, "gamma", "SKILL.md"), "---\ndescription: G\n---\ngamma body")

	list := m.List()
	if len(list) != 3 {
		t.Fatalf("list = %+v, want 3 skills", list)
	}
	for _, want := range list {
		got, err := m.Read(want.Name)
		if err != nil {
			t.Fatalf("Read(%q) from List: %v", want.Name, err)
		}
		if got != want {
			t.Fatalf("Read(%q) = %+v, List entry %+v", want.Name, got, want)
		}
	}
}

// TestReadNotLimitedByMaxSkills pins the behaviour delta of the direct-path
// lookup: Read is a single-skill lookup, so a name List drops past the
// MaxSkills cap is still readable (the old List-driven Read reported "not
// found" for exactly that skill).
func TestReadNotLimitedByMaxSkills(t *testing.T) {
	m, projSkills, _ := readFixture(t)
	// Sorted, skill-100 is the last name, so the cap (100) drops it from List.
	for i := 0; i <= MaxSkills; i++ {
		writeSkill(t, filepath.Join(projSkills, fmt.Sprintf("skill-%03d.md", i)), "body")
	}
	dropped := fmt.Sprintf("skill-%03d", MaxSkills)

	if list := m.List(); len(list) != MaxSkills {
		t.Fatalf("list = %d skills, want the MaxSkills cap %d", len(list), MaxSkills)
	}
	for _, s := range m.List() {
		if s.Name == dropped {
			t.Fatalf("List unexpectedly contains %q past the cap", dropped)
		}
	}
	got, err := m.Read(dropped)
	if err != nil {
		t.Fatalf("Read(%q) must not be limited by MaxSkills: %v", dropped, err)
	}
	if got.Name != dropped || got.Body != "body" || got.Source != "project" {
		t.Fatalf("Read(%q) = %+v", dropped, got)
	}
}

// TestReadGlobalModeUserRootOnly pins that Read follows the manager's
// global-mode root policy: only the user root is consulted.
func TestReadGlobalModeUserRootOnly(t *testing.T) {
	_, proj := mkProject(t)
	xdg := filepath.Join(t.TempDir(), "cfg")
	t.Setenv("XDG_CONFIG_HOME", xdg)
	writeSkill(t, filepath.Join(proj, ".gogen", "skills", "proj.md"), "project body")
	writeSkill(t, filepath.Join(xdg, "gogen", "skills", "user.md"), "user body")

	m := NewManager(proj, true)
	got, err := m.Read("user")
	if err != nil {
		t.Fatalf("Read(user): %v", err)
	}
	if got.Body != "user body" || got.Source != "user" {
		t.Fatalf("Read(user) = %+v", got)
	}
	if _, err := m.Read("proj"); err == nil {
		t.Fatal("global mode must not read the project root")
	}
}
