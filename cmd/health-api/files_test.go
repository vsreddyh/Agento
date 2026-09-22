package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveExport(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EXPORTS_DIR", root)
	if err := os.MkdirAll(filepath.Join(root, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveExport("sub"); err != nil {
		t.Fatalf("sub: %v", err)
	}
	if _, err := resolveExport(""); err != nil {
		t.Fatalf("root: %v", err)
	}
	for _, bad := range []string{"..", "../etc", "sub/../../x", "/absolute"} {
		if _, err := resolveExport(bad); err == nil {
			t.Fatalf("expected rejection for %q", bad)
		}
	}
}

func TestListAndDownload(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EXPORTS_DIR", root)
	t.Setenv("PASSWORD", "test-secret-12345678")
	if err := os.WriteFile(filepath.Join(root, "hello.txt"), []byte("hi"), 0o644); err != nil {
		t.Fatal(err)
	}
	auth := "Bearer test-secret-12345678"

	req := httptest.NewRequest(http.MethodGet, "/api/files?path=", nil)
	req.Header.Set("Authorization", auth)
	w := httptest.NewRecorder()
	listFiles(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), "hello.txt") {
		t.Fatalf("missing file: %s", w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/files/download?path=hello.txt", nil)
	req.Header.Set("Authorization", auth)
	w = httptest.NewRecorder()
	downloadFile(w, req)
	if w.Code != http.StatusOK || w.Body.String() != "hi" {
		t.Fatalf("download: %d %q", w.Code, w.Body.String())
	}

	req = httptest.NewRequest(http.MethodGet, "/api/files/download?path=../x", nil)
	req.Header.Set("Authorization", auth)
	w = httptest.NewRecorder()
	downloadFile(w, req)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("traversal: %d", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/files", nil)
	w = httptest.NewRecorder()
	listFiles(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth: %d", w.Code)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
