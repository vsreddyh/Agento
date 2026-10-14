package crashlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestFileNameShape(t *testing.T) {
	got := FileName(time.Date(2026, 10, 14, 3, 4, 5, 0, time.UTC))
	if got != "crash-20261014-030405.log" {
		t.Fatalf("got %q", got)
	}
}

func TestWriteAndPrune(t *testing.T) {
	dir := t.TempDir()
	path, err := WriteCrash(dir, "boom", "s3cret")
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if base := filepath.Base(path); base != "crash-"+time.Now().Format(timeFormat)+".log" {
		t.Fatalf("unexpected name %q", base)
	}
	// Same-second collision dedupes rather than overwriting.
	second, err := WriteCrash(dir, "boom again")
	if err != nil {
		t.Fatalf("second write: %v", err)
	}
	if second == path {
		t.Fatalf("collision overwrote %q", path)
	}
	// Fresh files survive pruning.
	if err := Prune(dir); err != nil {
		t.Fatalf("prune: %v", err)
	}
	for _, p := range []string{path, second} {
		if _, err := os.Stat(p); err != nil {
			t.Fatalf("fresh file pruned: %v", err)
		}
	}
	// A 6-day-old crash file is pruned; neighbours are untouched.
	old := filepath.Join(dir, "crash-20200101-000000.log")
	if err := os.WriteFile(old, []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	ancient := time.Now().Add(-6 * 24 * time.Hour)
	if err := os.Chtimes(old, ancient, ancient); err != nil {
		t.Fatal(err)
	}
	keep := filepath.Join(dir, "notes.txt")
	if err := os.WriteFile(keep, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Prune(dir); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Fatalf("old crash file survived")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Fatalf("non-crash file pruned: %v", err)
	}
}

func TestRedact(t *testing.T) {
	text := "password=hunter2 uri=mongodb+srv://u:p@h/ auth Authorization: Bearer abcdefgh12345678 done"
	got := Redact(text, "hunter2", "mongodb+srv://u:p@h/")
	if strings.Contains(got, "hunter2") || strings.Contains(got, "mongodb+srv") {
		t.Fatalf("secret leaked: %q", got)
	}
	if strings.Contains(got, "abcdefgh12345678") {
		t.Fatalf("bearer leaked: %q", got)
	}
	if !strings.Contains(got, "Authorization: Bearer ***") {
		t.Fatalf("header shape lost: %q", got)
	}
	// Empty secrets are skipped, and prose is left alone.
	if got := Redact("nothing here", ""); got != "nothing here" {
		t.Fatalf("got %q", got)
	}
	if got := Redact("bring your Bearer token", ""); got != "bring your Bearer token" {
		t.Fatalf("prose mangled: %q", got)
	}
}

func TestPruneMissingDir(t *testing.T) {
	if err := Prune(filepath.Join(t.TempDir(), "absent")); err != nil {
		t.Fatalf("missing dir should not error: %v", err)
	}
}
