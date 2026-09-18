package agent

import (
	"context"
	"testing"

	"gogen/internal/llm"
)

// refStubProvider records the (profile, ID) pair SelectModel selects, so
// the qualified-selector resolution is testable without a live catalog.
type refStubProvider struct {
	statsStubProvider
	ref llm.ModelRef
	set bool
}

func (s *refStubProvider) SetModelRef(ref llm.ModelRef) error {
	s.ref = ref
	s.set = true
	return nil
}

func (s *refStubProvider) CurrentModelRef() llm.ModelRef {
	if s.set {
		return s.ref
	}
	return llm.ModelRef{ID: s.ModelName()}
}

// TestSelectModelQualifiedSelector pins the per-profile selection contract:
// model IDs are not unique across registered providers, so the selection
// resolves to a (profile, ID) pair. A qualified "profile/id" selector picks
// THAT profile's copy of the ID; a bare selector resolves to the first
// catalog entry that matches (profile order — the default profile first);
// a numeric selector picks the list position (which carries its profile).
func TestSelectModelQualifiedSelector(t *testing.T) {
	models := []llm.ModelInfo{
		{ID: "gpt-4o", Provider: "default"},
		{ID: "shared-model", Provider: "default"},
		{ID: "llama3.1", Provider: "local"},
		{ID: "shared-model", Provider: "local"},
	}
	cases := []struct {
		name     string
		selector string
		wantRef  llm.ModelRef
		wantErr  bool
	}{
		{
			name:     "qualified selector picks the named profile's copy",
			selector: "local/shared-model",
			wantRef:  llm.ModelRef{Profile: "local", ID: "shared-model"},
		},
		{
			name:     "bare selector resolves to the first-listing profile",
			selector: "shared-model",
			wantRef:  llm.ModelRef{Profile: "default", ID: "shared-model"},
		},
		{
			name:     "numeric selector carries the list position's profile",
			selector: "4",
			wantRef:  llm.ModelRef{Profile: "local", ID: "shared-model"},
		},
		{
			name:     "default-profile model selects with the default profile",
			selector: "gpt-4o",
			wantRef:  llm.ModelRef{Profile: "default", ID: "gpt-4o"},
		},
		{
			name:     "unknown selector errors",
			selector: "nope",
			wantErr:  true,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := &refStubProvider{statsStubProvider: statsStubProvider{limit: 1000, models: models}}
			a := &Agent{Provider: p}
			err := a.SelectModel(context.Background(), tc.selector)
			if tc.wantErr {
				if err == nil {
					t.Fatal("expected an error for an unknown selector")
				}
				return
			}
			if err != nil {
				t.Fatalf("SelectModel(%q): %v", tc.selector, err)
			}
			if !p.set {
				t.Fatal("SelectModel must set the model via SetModelRef")
			}
			if p.ref != tc.wantRef {
				t.Fatalf("SelectModel(%q) = %+v, want %+v", tc.selector, p.ref, tc.wantRef)
			}
		})
	}
}

// TestListModelsCurrentFlagPerProfile pins the per-(profile, ID) current
// flag: the same model ID listed by several providers marks only the copy
// the session actually runs on.
func TestListModelsCurrentFlagPerProfile(t *testing.T) {
	p := &refStubProvider{
		statsStubProvider: statsStubProvider{limit: 1000, models: []llm.ModelInfo{
			{ID: "shared-model", Provider: "default"},
			{ID: "shared-model", Provider: "local"},
		}},
	}
	p.ref = llm.ModelRef{Profile: "local", ID: "shared-model"}
	p.set = true
	a := &Agent{Provider: p}

	models, err := a.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 2 {
		t.Fatalf("got %d models, want 2", len(models))
	}
	if models[0].Current {
		t.Fatal("the default-profile copy must not be marked current")
	}
	if !models[1].Current {
		t.Fatal("the local-profile copy (the one the session runs) must be marked current")
	}
}
