package agento_test

// The MCP launchers are the live launch path, so their failure modes are
// pinned by behaviour, not only by reading text. #557: a git-ignored `bin/`
// plus a manual build meant a pull-and-restart kept running the old binary —
// the live money server stuck on an 11-tool surface and a removed 90-day TTL
// long after main had moved on. `mcp-run.sh` must refuse instead — but only
// for sources that server builds on, and for every file type baked into the
// binary (including //go:embed assets), not just *.go.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeCheckout copies the real launcher into a throwaway repo root with a
// fake <server> binary that touches $MARKER when exec'd. Callers create
// source files under root, set their mtimes, and run the launcher.
type fakeCheckout struct {
	t      *testing.T
	root   string
	server string
	bin    string
	marker string
}

func newFakeCheckout(t *testing.T, server string) *fakeCheckout {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("launcher is bash")
	}
	root := t.TempDir()
	for _, f := range []string{"scripts/mcp-run.sh", "scripts/lib/common.sh"} {
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("%s: %v", f, err)
		}
		dst := filepath.Join(root, f)
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(dst, raw, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".env"),
		[]byte("MONGODB_URI=mongodb://127.0.0.1:27017/fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	marker := filepath.Join(root, "ran")
	bin := filepath.Join(root, "bin", server)
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n: > \"$MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeCheckout{t: t, root: root, server: server, bin: bin, marker: marker}
}

func (c *fakeCheckout) write(rel, content string) string {
	c.t.Helper()
	p := filepath.Join(c.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		c.t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		c.t.Fatal(err)
	}
	return p
}

func (c *fakeCheckout) chtimes(rel string, t time.Time) {
	c.t.Helper()
	if err := os.Chtimes(filepath.Join(c.root, rel), t, t); err != nil {
		c.t.Fatal(err)
	}
}

func (c *fakeCheckout) run() ([]byte, error) {
	c.t.Helper()
	cmd := exec.Command("bash", filepath.Join(c.root, "scripts", "mcp-run.sh"), c.server)
	cmd.Dir = c.root
	cmd.Env = append(os.Environ(), "MARKER="+c.marker)
	return cmd.CombinedOutput()
}

func (c *fakeCheckout) expectRefuse(t *testing.T, wantNames ...string) {
	t.Helper()
	out, err := c.run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("launch = %v (out %q), want exit 1", err, out)
	}
	for _, want := range append([]string{"stale", "scripts/build-mcps.sh"}, wantNames...) {
		if !strings.Contains(string(out), want) {
			t.Errorf("refusal %q does not mention %q", out, want)
		}
	}
	if _, err := os.Stat(c.marker); !os.IsNotExist(err) {
		t.Fatalf("stale binary was exec'd (marker %v) — the guard must run before exec", err)
	}
}

func (c *fakeCheckout) expectLaunch(t *testing.T) {
	t.Helper()
	os.Remove(c.marker)
	if out, err := c.run(); err != nil {
		t.Fatalf("launch = %v (out %q), want success", err, out)
	}
	if _, err := os.Stat(c.marker); err != nil {
		t.Fatalf("fresh launch did not exec the binary: %v", err)
	}
}

// seed writes the files a server's guard watches (own cmd dir, own store
// package, its shared libs, go.mod/go.sum) at `at` — callers move individual
// files newer afterwards.
func (c *fakeCheckout) seed(at time.Time, own string, shared []string) {
	c.t.Helper()
	rels := []string{"cmd/" + c.server + "/main.go", "internal/" + own + "/store.go", "go.mod", "go.sum"}
	for _, s := range shared {
		rels = append(rels, "internal/"+s+"/lib.go")
	}
	for _, rel := range rels {
		c.write(rel, "package fake\n")
		c.chtimes(rel, at)
	}
	c.chtimes("bin/"+c.server, at)
}

func (c *fakeCheckout) git(t *testing.T, date time.Time, args ...string) {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = c.root
	stamp := date.Format("2006-01-02T15:04:05Z07:00")
	cmd.Env = append(os.Environ(),
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
		"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t",
		"GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v (%s)", args, err, out)
	}
}

// TestMCPRunRefusesStaleBinary pins the guard in both directions with mtimes
// on a throwaway checkout: watched sources refuse before exec, unrelated or
// test-only changes launch normally.
func TestMCPRunRefusesStaleBinary(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-time.Hour)

	t.Run("own store source refuses", func(t *testing.T) {
		c := newFakeCheckout(t, "miser-money")
		c.seed(old, "money", []string{"mongo", "mongostore", "validate"})
		c.write("internal/money/store.go", "package fake\n// changed\n")
		c.chtimes("internal/money/store.go", newer)
		c.expectRefuse(t, "store.go")
	})

	t.Run("own cmd source refuses", func(t *testing.T) {
		c := newFakeCheckout(t, "miser-money")
		c.seed(old, "money", []string{"mongo", "mongostore", "validate"})
		c.chtimes("cmd/miser-money/main.go", newer)
		c.expectRefuse(t, "main.go")
	})

	t.Run("non-Go embedded asset refuses", func(t *testing.T) {
		c := newFakeCheckout(t, "miser-money")
		c.seed(old, "money", []string{"mongo", "mongostore", "validate"})
		// e.g. internal/reports/katex.min.js rides into its binary via
		// //go:embed; a *.go-only scan would miss it entirely.
		c.write("internal/money/seed.json", "{}\n")
		c.chtimes("internal/money/seed.json", newer)
		c.expectRefuse(t, "seed.json")
	})

	t.Run("imported shared lib refuses", func(t *testing.T) {
		c := newFakeCheckout(t, "miser-money")
		c.seed(old, "money", []string{"mongo", "mongostore", "validate"})
		c.chtimes("internal/mongo/lib.go", newer)
		c.expectRefuse(t, "lib.go")
	})

	t.Run("unimported shared lib does not block", func(t *testing.T) {
		// cookbook imports mongo+validate, not mongostore.
		c := newFakeCheckout(t, "cookbook")
		c.seed(old, "cookbook", []string{"mongo", "validate"})
		c.write("internal/mongostore/mongostore.go", "package fake\n")
		c.chtimes("internal/mongostore/mongostore.go", newer)
		c.expectLaunch(t)
	})

	t.Run("reports ignores validate", func(t *testing.T) {
		// reports imports only mongo besides its own package.
		c := newFakeCheckout(t, "reports")
		c.seed(old, "reports", []string{"mongo"})
		c.write("internal/validate/validate.go", "package fake\n")
		c.chtimes("internal/validate/validate.go", newer)
		c.expectLaunch(t)
	})

	t.Run("reports watches mongo", func(t *testing.T) {
		c := newFakeCheckout(t, "reports")
		c.seed(old, "reports", []string{"mongo"})
		c.chtimes("internal/mongo/lib.go", newer)
		c.expectRefuse(t, "lib.go")
	})

	t.Run("unrelated cmd dir does not block", func(t *testing.T) {
		c := newFakeCheckout(t, "miser-money")
		c.seed(old, "money", []string{"mongo", "mongostore", "validate"})
		c.write("cmd/health-api/main.go", "package fake\n")
		c.chtimes("cmd/health-api/main.go", newer)
		c.expectLaunch(t)
	})

	t.Run("other server store does not block", func(t *testing.T) {
		c := newFakeCheckout(t, "miser-money")
		c.seed(old, "money", []string{"mongo", "mongostore", "validate"})
		c.write("internal/cookbook/store.go", "package fake\n")
		c.chtimes("internal/cookbook/store.go", newer)
		c.expectLaunch(t)
	})

	t.Run("test-only change does not block", func(t *testing.T) {
		c := newFakeCheckout(t, "miser-money")
		c.seed(old, "money", []string{"mongo", "mongostore", "validate"})
		c.write("internal/money/store_test.go", "package fake\n")
		c.chtimes("internal/money/store_test.go", newer)
		c.expectLaunch(t)
	})

	t.Run("fresh binary launches", func(t *testing.T) {
		c := newFakeCheckout(t, "miser-money")
		c.seed(old, "money", []string{"mongo", "mongostore", "validate"})
		c.chtimes("bin/miser-money", time.Now())
		c.expectLaunch(t)
	})
}

// TestMCPRunRefusesMergeBroughtDeletion pins the merge edge the plain
// `git log -- <paths>` HEAD check missed: a merge that only brings a
// watched-path deletion is TREESAME to its feature-branch parent for those
// paths, so default history simplification prunes the merge and log -1
// returns the older feature commit. A binary built between the feature commit
// and the merge then looks fresh and gets exec'd. The launcher passes
// -m --full-history so the merge commit itself is compared.
func TestMCPRunRefusesMergeBroughtDeletion(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c := newFakeCheckout(t, "miser-money")
	base := time.Now().Add(-3 * time.Hour)
	c.seed(base, "money", []string{"mongo", "mongostore", "validate"})
	c.write("internal/money/doomed.go", "package fake\n")
	c.git(t, base, "init", "-q")
	c.git(t, base, "add", "-A")
	c.git(t, base, "commit", "-qm", "seed")
	c.git(t, base, "checkout", "-qb", "feature")
	if err := os.Remove(filepath.Join(c.root, "internal/money/doomed.go")); err != nil {
		t.Fatal(err)
	}
	featAt := base.Add(80 * time.Minute)
	c.git(t, featAt, "add", "-A")
	c.git(t, featAt, "commit", "-qm", "delete doomed")
	c.git(t, featAt, "checkout", "-q", "-")
	// Master advances outside the watched paths, so the merge stays TREESAME
	// to the feature parent for those paths — the shape that pruned it.
	c.write("README.md", "docs\n")
	c.git(t, base.Add(100*time.Minute), "add", "-A")
	c.git(t, base.Add(100*time.Minute), "commit", "-qm", "docs advance")
	// Binary "built" after the feature commit but before the merge: it still
	// contains doomed.go.
	c.chtimes("bin/miser-money", base.Add(90*time.Minute))
	mergeAt := base.Add(160 * time.Minute)
	c.git(t, mergeAt, "merge", "-q", "--no-ff", "feature", "-m", "merge feature")
	// Only the HEAD check can fire: normalize surviving mtimes older than the
	// binary, the way the deletion test isolates it.
	for _, rel := range []string{
		"cmd/miser-money/main.go", "internal/money/store.go",
		"internal/mongo/lib.go", "internal/mongostore/lib.go",
		"internal/validate/lib.go", "go.mod", "go.sum", "README.md",
	} {
		c.chtimes(rel, base)
	}
	c.expectRefuse(t, "git HEAD")
}

// TestMCPRunGitIgnoresTestOnlyMerge is the control for the merge edge: a
// merge bringing only a *_test.go change must NOT refuse, matching the mtime
// scan's exclusion.
func TestMCPRunGitIgnoresTestOnlyMerge(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c := newFakeCheckout(t, "miser-money")
	base := time.Now().Add(-3 * time.Hour)
	c.seed(base, "money", []string{"mongo", "mongostore", "validate"})
	c.git(t, base, "init", "-q")
	c.git(t, base, "add", "-A")
	c.git(t, base, "commit", "-qm", "seed")
	c.git(t, base, "checkout", "-qb", "feature")
	c.write("internal/money/store_test.go", "package fake\n// changed\n")
	c.git(t, base.Add(80*time.Minute), "add", "-A")
	c.git(t, base.Add(80*time.Minute), "commit", "-qm", "test only")
	c.git(t, base.Add(80*time.Minute), "checkout", "-q", "-")
	c.write("README.md", "docs\n")
	c.git(t, base.Add(100*time.Minute), "add", "-A")
	c.git(t, base.Add(100*time.Minute), "commit", "-qm", "docs advance")
	c.chtimes("bin/miser-money", base.Add(90*time.Minute))
	c.git(t, base.Add(160*time.Minute), "merge", "-q", "--no-ff", "feature", "-m", "merge tests")
	for _, rel := range []string{
		"cmd/miser-money/main.go", "internal/money/store.go",
		"internal/mongo/lib.go", "internal/mongostore/lib.go",
		"internal/validate/lib.go", "go.mod", "go.sum", "README.md",
	} {
		c.chtimes(rel, base)
	}
	c.expectLaunch(t)
}

// TestMCPRunRefusesDeletedSource pins the deletion edge: a commit that only
// deletes a watched file leaves no newer file behind, so the mtime scan
// passes — but the git HEAD check must still refuse.
func TestMCPRunRefusesDeletedSource(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c := newFakeCheckout(t, "miser-money")
	base := time.Now().Add(-3 * time.Hour)
	c.seed(base, "money", []string{"mongo", "mongostore", "validate"})
	c.write("internal/money/doomed.go", "package fake\n")
	c.git(t, base, "init", "-q")
	c.git(t, base, "add", "-A")
	c.git(t, base, "commit", "-qm", "seed")
	// Binary "rebuilt" after the seed commit.
	c.chtimes("bin/miser-money", base.Add(2*time.Hour))
	if err := os.Remove(filepath.Join(c.root, "internal/money/doomed.go")); err != nil {
		t.Fatal(err)
	}
	c.git(t, base.Add(150*time.Minute), "add", "-A")
	c.git(t, base.Add(150*time.Minute), "commit", "-qm", "drop doomed")
	// No file is newer than the binary now — only HEAD knows.
	c.expectRefuse(t, "git HEAD")
}

// TestMCPRunGitIgnoresTestOnlyCommit pins the other side of the HEAD check: a
// commit touching only *_test.go under a watched dir must NOT refuse, matching
// the mtime scan's exclusion.
func TestMCPRunGitIgnoresTestOnlyCommit(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c := newFakeCheckout(t, "miser-money")
	base := time.Now().Add(-3 * time.Hour)
	c.seed(base, "money", []string{"mongo", "mongostore", "validate"})
	c.git(t, base, "init", "-q")
	c.git(t, base, "add", "-A")
	c.git(t, base, "commit", "-qm", "seed")
	// Binary "rebuilt" after the seed commit.
	c.chtimes("bin/miser-money", base.Add(2*time.Hour))
	c.write("internal/money/store_test.go", "package fake\n// changed\n")
	c.git(t, base.Add(150*time.Minute), "add", "-A")
	c.git(t, base.Add(150*time.Minute), "commit", "-qm", "test only")
	c.expectLaunch(t)
}

// wantWatch is the exact internal closure each MCP server builds on: its own
// store package plus the shared libs it imports. It must match both `go list
// -deps` below and the case mapping in scripts/mcp-run.sh — a new import
// without a mapping update fails this test instead of serving stale tools.
var wantWatch = map[string][]string{
	"task-manager":    {"mongo", "mongostore", "tasks", "validate"},
	"project-manager": {"mongo", "mongostore", "projects"},
	"miser-money":     {"money", "mongo", "mongostore", "validate"},
	"cookbook":        {"cookbook", "mongo", "validate"},
	"health-check":    {"healthcheck", "mongo", "validate"},
	"reports":         {"mongo", "reports"},
}

// TestMCPWatchSetsMatchGoDeps pins the launcher's per-server watch sets to
// the actual transitive imports. If a server gains or drops an internal
// import, update the case mapping in scripts/mcp-run.sh in the same PR.
func TestMCPWatchSetsMatchGoDeps(t *testing.T) {
	launcher, err := os.ReadFile("scripts/mcp-run.sh")
	if err != nil {
		t.Fatalf("read launcher: %v", err)
	}
	for server, want := range wantWatch {
		out, err := exec.Command("go", "list", "-f", "{{ join .Deps \"\\n\" }}", "./cmd/"+server).Output()
		if err != nil {
			t.Fatalf("go list ./cmd/%s: %v", server, err)
		}
		var got []string
		for _, dep := range strings.Split(strings.TrimSpace(string(out)), "\n") {
			if rest, ok := strings.CutPrefix(dep, "agento/internal/"); ok {
				got = append(got, rest)
			}
		}
		sort.Strings(got)
		sorted := append([]string(nil), want...)
		sort.Strings(sorted)
		if strings.Join(got, ",") != strings.Join(sorted, ",") {
			t.Errorf("cmd/%s internal deps = %v, want %v — update wantWatch and scripts/mcp-run.sh together", server, got, sorted)
		}
		// The launcher's case arm must name this server's set.
		for _, pkg := range want {
			if !strings.Contains(string(launcher), pkg) {
				t.Errorf("launcher does not mention internal/%s (needed by %s)", pkg, server)
			}
		}
	}
}
