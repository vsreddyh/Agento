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
	"strings"
	"testing"
	"time"
)

// fakeCheckout copies the real launcher into a throwaway repo root with a
// fake miser-money binary that touches $MARKER when exec'd. Callers create
// source files under root, set their mtimes, and run the launcher.
type fakeCheckout struct {
	t      *testing.T
	root   string
	bin    string
	marker string
}

func newFakeCheckout(t *testing.T) *fakeCheckout {
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
	bin := filepath.Join(root, "bin", "miser-money")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n: > \"$MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return &fakeCheckout{t: t, root: root, bin: bin, marker: marker}
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
	cmd := exec.Command("bash", filepath.Join(c.root, "scripts", "mcp-run.sh"), "miser-money")
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

// seedMiser writes the files the miser-money guard watches (own cmd dir, own
// store package, shared libs, go.mod/go.sum) at `at`, and returns nothing —
// callers move individual files newer afterwards.
func (c *fakeCheckout) seedMiser(at time.Time) {
	c.t.Helper()
	for _, rel := range []string{
		"cmd/miser-money/main.go",
		"internal/money/store.go",
		"internal/mongo/mongo.go",
		"internal/mongostore/mongostore.go",
		"internal/validate/validate.go",
		"go.mod",
		"go.sum",
	} {
		c.write(rel, "package fake\n")
		c.chtimes(rel, at)
	}
	c.chtimes("bin/miser-money", at)
}

// TestMCPRunRefusesStaleBinary pins the guard in both directions with mtimes
// on a throwaway checkout: watched sources refuse before exec, unrelated or
// test-only changes launch normally.
func TestMCPRunRefusesStaleBinary(t *testing.T) {
	old := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-time.Hour)

	t.Run("own store source refuses", func(t *testing.T) {
		c := newFakeCheckout(t)
		c.seedMiser(old)
		c.write("internal/money/store.go", "package fake\n// changed\n")
		c.chtimes("internal/money/store.go", newer)
		c.expectRefuse(t, "store.go")
	})

	t.Run("own cmd source refuses", func(t *testing.T) {
		c := newFakeCheckout(t)
		c.seedMiser(old)
		c.chtimes("cmd/miser-money/main.go", newer)
		c.expectRefuse(t, "main.go")
	})

	t.Run("non-Go embedded asset refuses", func(t *testing.T) {
		c := newFakeCheckout(t)
		c.seedMiser(old)
		// e.g. internal/reports/katex.min.js rides into its binary via
		// //go:embed; a *.go-only scan would miss it entirely.
		c.write("internal/money/seed.json", "{}\n")
		c.chtimes("internal/money/seed.json", newer)
		c.expectRefuse(t, "seed.json")
	})

	t.Run("shared lib refuses", func(t *testing.T) {
		c := newFakeCheckout(t)
		c.seedMiser(old)
		c.chtimes("internal/mongo/mongo.go", newer)
		c.expectRefuse(t, "mongo.go")
	})

	t.Run("unrelated cmd dir does not block", func(t *testing.T) {
		c := newFakeCheckout(t)
		c.seedMiser(old)
		c.write("cmd/health-api/main.go", "package fake\n")
		c.chtimes("cmd/health-api/main.go", newer)
		c.expectLaunch(t)
	})

	t.Run("other server store does not block", func(t *testing.T) {
		c := newFakeCheckout(t)
		c.seedMiser(old)
		c.write("internal/cookbook/store.go", "package fake\n")
		c.chtimes("internal/cookbook/store.go", newer)
		c.expectLaunch(t)
	})

	t.Run("test-only change does not block", func(t *testing.T) {
		c := newFakeCheckout(t)
		c.seedMiser(old)
		c.write("internal/money/store_test.go", "package fake\n")
		c.chtimes("internal/money/store_test.go", newer)
		c.expectLaunch(t)
	})

	t.Run("fresh binary launches", func(t *testing.T) {
		c := newFakeCheckout(t)
		c.seedMiser(old)
		c.chtimes("bin/miser-money", time.Now())
		c.expectLaunch(t)
	})
}

// TestMCPRunRefusesDeletedSource pins the deletion edge: a commit that only
// deletes a watched file leaves no newer file behind, so the mtime scan
// passes — but the git HEAD check must still refuse.
func TestMCPRunRefusesDeletedSource(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("launcher is bash")
	}
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git not available")
	}
	c := newFakeCheckout(t)
	now := time.Now()
	c.seedMiser(now.Add(-3 * time.Hour))
	c.write("internal/money/doomed.go", "package fake\n")
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = c.root
		cmd.Env = append(os.Environ(),
			"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t",
			"GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v (%s)", args, err, out)
		}
	}
	run("init", "-q")
	run("add", "-A")
	run("commit", "-qm", "seed")
	// Binary "rebuilt" after the seed commit.
	c.chtimes("bin/miser-money", time.Now().Add(-time.Hour))
	if err := os.Remove(filepath.Join(c.root, "internal/money/doomed.go")); err != nil {
		t.Fatal(err)
	}
	run("add", "-A")
	run("commit", "-qm", "drop doomed")
	// No file is newer than the binary now — only HEAD knows.
	c.expectRefuse(t, "git HEAD")
}
