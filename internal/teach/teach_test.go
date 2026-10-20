package teach

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNameValidation(t *testing.T) {
	// ValidName trims: surrounding whitespace is fine (stored trimmed),
	// inner structure is what matters.
	for _, good := range []string{"weekly-report", "a", "a1", "0-9", " weekly "} {
		if !ValidName(good) {
			t.Fatalf("ValidName(%q) = false", good)
		}
	}
	for _, bad := range []string{"", "A", "a_b", "a/b", "../x", "-a", "a-", "Work Style", "a b"} {
		if ValidName(bad) {
			t.Fatalf("ValidName(%q) = true", bad)
		}
	}
}

func TestRenderSKILLMatchesCommittedShape(t *testing.T) {
	got := RenderSKILL(Skill{Name: "weekly", Description: `A "quoted" desc`, Body: "# Weekly\nDo it."})
	if !strings.HasPrefix(got, "---\nname: weekly\n") {
		t.Fatalf("frontmatter:\n%s", got)
	}
	if !strings.Contains(got, `description: "A \"quoted\" desc"`) {
		t.Fatalf("description quoting:\n%s", got)
	}
	if !strings.HasSuffix(got, "# Weekly\nDo it.\n") {
		t.Fatalf("body:\n%s", got)
	}
}

func TestMemoryLifecycle(t *testing.T) {
	ctx := context.Background()
	s := NewMemoryStore()

	// names are strict on write: "Weekly" is rejected, not coerced
	if _, err := s.Create(ctx, Skill{Profile: "p", Name: "Weekly", Description: "x", Body: "y"}); err == nil {
		t.Fatal("uppercase name accepted")
	}
	// the second twin fails: names are unique per profile, and this pins
	// the duplicate path rather than the validation path
	if _, err := s.Create(ctx, Skill{Profile: "p", Name: "twin", Description: "first", Body: "1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Create(ctx, Skill{Profile: "p", Name: "twin", Description: "second", Body: "2"}); err == nil {
		t.Fatal("duplicate name accepted")
	}
	a, err := s.Create(ctx, Skill{Profile: "p", Name: "weekly", Description: "  does things  ", Body: "do it"})
	if err != nil {
		t.Fatal(err)
	}
	// the lifecycle starts at draft, always
	if a.Name != "weekly" || a.Status != StatusDraft || a.Description != "does things" {
		t.Fatalf("create = %+v", a)
	}
	if _, err := s.Create(ctx, Skill{Profile: "q", Name: "weekly", Description: "x", Body: "y"}); err != nil {
		t.Fatalf("cross-profile name blocked: %v", err)
	}
	// activate without a test fails closed
	if _, err := s.SetStatus(ctx, a.ID, "p", StatusActive); err == nil {
		t.Fatal("untested activate accepted")
	}
	tested, err := s.RecordTest(ctx, a.ID, "p", 7, "ran fine")
	if err != nil || tested.TestedAt != 7 || tested.TestText != "ran fine" {
		t.Fatalf("record test = %+v %v", tested, err)
	}
	active, err := s.SetStatus(ctx, a.ID, "p", StatusActive)
	if err != nil || active.Status != StatusActive {
		t.Fatalf("activate = %+v %v", active, err)
	}
	// foreign profile reads 404
	if _, err := s.Get(ctx, a.ID, "q"); err == nil {
		t.Fatal("foreign get accepted")
	}
	if err := s.Delete(ctx, a.ID, "q"); err == nil {
		t.Fatal("foreign delete accepted")
	}
	if err := s.Delete(ctx, a.ID, "p"); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializeReconciles(t *testing.T) {
	dir := t.TempDir()
	n, skipped, err := Materialize(dir, []Skill{
		{Profile: "p", Name: "b-skill", Description: "b", Body: "B", Status: StatusActive},
		{Profile: "p", Name: "a-skill", Description: "a", Body: "A", Status: StatusActive},
		{Profile: "p", Name: "draft", Description: "d", Body: "D", Status: StatusDraft},
		{Profile: "p", Name: "off", Description: "o", Body: "O", Status: StatusDisabled},
	})
	if err != nil || n != 2 || len(skipped) != 0 {
		t.Fatalf("materialize = %d, %v, skipped %v", n, err, skipped)
	}
	b, err := os.ReadFile(filepath.Join(dir, "a-skill", "SKILL.md"))
	if err != nil || !strings.Contains(string(b), "name: a-skill") {
		t.Fatalf("skill file = %q, %v", b, err)
	}
	// a stale directory from a disabled skill is removed; stray files stay
	stale := filepath.Join(dir, "off")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, skipped, err := Materialize(dir, nil); err != nil || len(skipped) != 0 {
		t.Fatalf("empty reconcile = %v, skipped %v", err, skipped)
	}
	if _, err := os.Stat(stale); !os.IsNotExist(err) {
		t.Fatal("stale skill dir survived")
	}
	if _, err := os.Stat(filepath.Join(dir, "README.md")); err != nil {
		t.Fatal("stray file removed")
	}
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if e.IsDir() {
			t.Fatalf("dir %q survived an empty reconcile", e.Name())
		}
	}
}
