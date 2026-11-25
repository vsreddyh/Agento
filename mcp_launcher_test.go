package agento_test

// The MCP launchers are the live launch path, so their failure modes are
// pinned by behaviour, not only by reading text. #557: a git-ignored `bin/`
// plus a manual build meant a pull-and-restart kept running the old binary —
// the live money server stuck on an 11-tool surface and a removed 90-day TTL
// long after main had moved on. `mcp-run.sh` must refuse instead.

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

// TestMCPRunRefusesStaleBinary builds a throwaway checkout around a copy of
// the real launcher and a fake server binary, then moves mtimes instead of
// rebuilding anything:
//
//   - source newer than bin => exit 1 with the rebuild instructions, and the
//     binary is never exec'd (the marker file stays absent);
//   - bin newer than every source => the binary runs normally, so the guard
//     does not block the ordinary launch path.
func TestMCPRunRefusesStaleBinary(t *testing.T) {
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

	// A fresh checkout always has cmd/ and internal/; the guard scans both.
	source := filepath.Join(root, "internal", "money", "store.go")
	cmdSource := filepath.Join(root, "cmd", "fake", "main.go")
	for _, f := range []string{source, cmdSource, filepath.Join(root, "go.mod"), filepath.Join(root, "go.sum")} {
		if err := os.MkdirAll(filepath.Dir(f), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(f, []byte("package fake\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, ".env"),
		[]byte("MONGODB_URI=mongodb://127.0.0.1:27017/fake\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	marker := filepath.Join(root, "ran")
	bin := filepath.Join(root, "bin", "fake")
	if err := os.MkdirAll(filepath.Dir(bin), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, []byte("#!/bin/sh\n: > \"$MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}

	run := func() ([]byte, error) {
		cmd := exec.Command("bash", filepath.Join(root, "scripts", "mcp-run.sh"), "fake")
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "MARKER="+marker)
		return cmd.CombinedOutput()
	}

	// Source newer than the binary: refuse and do not exec.
	old := time.Now().Add(-2 * time.Hour)
	newer := time.Now().Add(-time.Hour)
	for _, f := range []string{bin, filepath.Join(root, "go.mod"), filepath.Join(root, "go.sum"), cmdSource} {
		if err := os.Chtimes(f, old, old); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.Chtimes(source, newer, newer); err != nil {
		t.Fatal(err)
	}
	out, err := run()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 1 {
		t.Fatalf("stale launch = %v (out %q), want exit 1", err, out)
	}
	for _, want := range []string{"stale", "scripts/build-mcps.sh", "store.go"} {
		if !strings.Contains(string(out), want) {
			t.Errorf("stale error %q does not mention %q", out, want)
		}
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("stale binary was exec'd (marker %v) — the guard must run before exec", err)
	}

	// Rebuilt binary: the normal launch path still works.
	if err := os.Chtimes(bin, time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	if out, err := run(); err != nil {
		t.Fatalf("fresh launch = %v (out %q), want success", err, out)
	}
	if _, err := os.Stat(marker); err != nil {
		t.Fatalf("fresh launch did not exec the binary: %v", err)
	}
}
