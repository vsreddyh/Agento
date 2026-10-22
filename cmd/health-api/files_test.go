package main

import (
	"bytes"
	"crypto/rand"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
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

// uploadRequest builds a multipart POST for /api/files/upload.
func uploadRequest(t *testing.T, url, field, name string, content []byte) *http.Request {
	t.Helper()
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	part, err := mw.CreateFormFile(field, name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, url, &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	return req
}

func TestUploadRoundTrip(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EXPORTS_DIR", root)
	t.Setenv("PASSWORD", "test-secret-12345678")
	if err := os.MkdirAll(filepath.Join(root, "receipts"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Random bytes, not text: the round trip must be byte-identical.
	content := make([]byte, 1<<20)
	if _, err := rand.Read(content); err != nil {
		t.Fatal(err)
	}

	req := uploadRequest(t, "/api/files/upload?path=receipts&name=scan.bin", "file", "scan.bin", content)
	w := httptest.NewRecorder()
	uploadFile(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("upload: %d %s", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), "scan.bin") {
		t.Fatalf("response names nothing: %s", w.Body.String())
	}

	// Downloading it back matches byte-for-byte.
	dl := httptest.NewRequest(http.MethodGet, "/api/files/download?path=receipts/scan.bin", nil)
	dl.Header.Set("Authorization", "Bearer test-secret-12345678")
	dw := httptest.NewRecorder()
	downloadFile(dw, dl)
	if dw.Code != http.StatusOK {
		t.Fatalf("download: %d %s", dw.Code, dw.Body.String())
	}
	if !bytes.Equal(dw.Body.Bytes(), content) {
		t.Fatal("round trip differs")
	}
}

func TestUploadConflictKeepsExisting(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EXPORTS_DIR", root)
	t.Setenv("PASSWORD", "test-secret-12345678")

	first := uploadRequest(t, "/api/files/upload?path=&name=dup.txt", "file", "dup.txt", []byte("first"))
	w := httptest.NewRecorder()
	uploadFile(w, first)
	if w.Code != http.StatusOK {
		t.Fatalf("first upload: %d %s", w.Code, w.Body.String())
	}
	second := uploadRequest(t, "/api/files/upload?path=&name=dup.txt", "file", "dup.txt", []byte("second"))
	w = httptest.NewRecorder()
	uploadFile(w, second)
	if w.Code != http.StatusConflict {
		t.Fatalf("second upload: %d %s, want 409", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), "file_exists") {
		t.Fatalf("conflict is not named: %s", w.Body.String())
	}
	kept, err := os.ReadFile(filepath.Join(root, "dup.txt"))
	if err != nil || string(kept) != "first" {
		t.Fatalf("existing file replaced: %q, %v", kept, err)
	}
}

func TestUploadOversizeRefusesNamed(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EXPORTS_DIR", root)
	t.Setenv("PASSWORD", "test-secret-12345678")

	big := bytes.Repeat([]byte("x"), maxUploadBytes+1)
	req := uploadRequest(t, "/api/files/upload?path=&name=big.bin", "file", "big.bin", big)
	w := httptest.NewRecorder()
	uploadFile(w, req)
	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversize: %d %s, want 413", w.Code, w.Body.String())
	}
	if !contains(w.Body.String(), "file_too_large") {
		t.Fatalf("refusal is not named: %s", w.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(root, "big.bin")); !os.IsNotExist(err) {
		t.Fatal("oversized upload left a file behind")
	}
}

func TestUploadRejectsEscapes(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EXPORTS_DIR", root)
	t.Setenv("PASSWORD", "test-secret-12345678")

	for _, url := range []string{
		"/api/files/upload?path=../x&name=evil.txt",
		"/api/files/upload?path=/absolute&name=evil.txt",
		"/api/files/upload?path=&name=../evil.txt",
		"/api/files/upload?path=&name=a/b.txt",
		"/api/files/upload?path=&name=",
		// Header injection through the download's Content-Disposition:
		// quotes and controls never become stored names.
		"/api/files/upload?path=&name=evil%22.txt",
		"/api/files/upload?path=&name=evil%0D%0A.txt",
		"/api/files/upload?path=&name=evil%7F.txt",
		// The staging prefix is reserved: such a file would store but
		// never list, with no delete endpoint to reclaim it.
		"/api/files/upload?path=&name=.upload-note.txt",
	} {
		req := uploadRequest(t, url, "file", "evil.txt", []byte("x"))
		w := httptest.NewRecorder()
		uploadFile(w, req)
		if w.Code != http.StatusBadRequest {
			t.Errorf("%s: got %d, want 400", url, w.Code)
		}
	}
	if _, err := os.Lstat(filepath.Join(root, "evil.txt")); !os.IsNotExist(err) {
		t.Error("escape wrote outside the target")
	}
	parent := filepath.Dir(root)
	if _, err := os.Lstat(filepath.Join(parent, "evil.txt")); !os.IsNotExist(err) {
		_ = os.Remove(filepath.Join(parent, "evil.txt"))
		t.Error("escape wrote outside the exports root")
	}
}

func TestUploadAuthAndMethod(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EXPORTS_DIR", root)
	t.Setenv("PASSWORD", "test-secret-12345678")

	req := uploadRequest(t, "/api/files/upload?path=&name=a.txt", "file", "a.txt", []byte("x"))
	req.Header.Del("Authorization")
	w := httptest.NewRecorder()
	uploadFile(w, req)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("no-auth: %d, want 401", w.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/api/files/upload?path=&name=a.txt", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	uploadFile(w, req)
	if w.Code != http.StatusMethodNotAllowed {
		t.Fatalf("GET: %d, want 405", w.Code)
	}

	// Missing file part is a named 400, not a 500.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	_ = mw.Close()
	req = httptest.NewRequest(http.MethodPost, "/api/files/upload?path=&name=a.txt", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w = httptest.NewRecorder()
	uploadFile(w, req)
	if w.Code != http.StatusBadRequest || !contains(w.Body.String(), "file_missing") {
		t.Fatalf("no part: %d %s", w.Code, w.Body.String())
	}
}

func TestUploadBodyCapRefusesNamed(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EXPORTS_DIR", root)
	t.Setenv("PASSWORD", "test-secret-12345678")

	// A small file buried behind megabytes of padding: the whole-request
	// cap fires during multipart parsing, before the file bytes are ever
	// staged — answered 413, not 400, with the named detail.
	var body bytes.Buffer
	mw := multipart.NewWriter(&body)
	if err := mw.WriteField("pad", string(bytes.Repeat([]byte("p"), maxUploadBytes+(1<<20)))); err != nil {
		t.Fatal(err)
	}
	part, err := mw.CreateFormFile("file", "tiny.txt")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := part.Write([]byte("tiny")); err != nil {
		t.Fatal(err)
	}
	if err := mw.Close(); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/files/upload?path=&name=tiny.txt", &body)
	req.Header.Set("Content-Type", mw.FormDataContentType())
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	uploadFile(w, req)
	if w.Code != http.StatusRequestEntityTooLarge || !contains(w.Body.String(), "file_too_large") {
		t.Fatalf("body cap: %d %s, want named 413", w.Code, w.Body.String())
	}
	if _, err := os.Lstat(filepath.Join(root, "tiny.txt")); !os.IsNotExist(err) {
		t.Fatal("capped upload left a file behind")
	}
}

func TestListHidesUploadStaging(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EXPORTS_DIR", root)
	t.Setenv("PASSWORD", "test-secret-12345678")
	if err := os.WriteFile(filepath.Join(root, ".upload-12345"), []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "real.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/files?path=", nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	listFiles(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("list: %d %s", w.Code, w.Body.String())
	}
	if contains(w.Body.String(), ".upload-12345") {
		t.Fatalf("staging file listed: %s", w.Body.String())
	}
	if !contains(w.Body.String(), "real.txt") {
		t.Fatalf("real file missing: %s", w.Body.String())
	}
}

func TestDownloadSanitizesStoredName(t *testing.T) {
	root := t.TempDir()
	t.Setenv("EXPORTS_DIR", root)
	t.Setenv("PASSWORD", "test-secret-12345678")
	// Planted outside the upload path (uploads reject these now): the
	// download must still serve a safe header.
	evil := "evil\".txt"
	if err := os.WriteFile(filepath.Join(root, evil), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/api/files/download?path="+url.QueryEscape(evil), nil)
	req.Header.Set("Authorization", "Bearer test-secret-12345678")
	w := httptest.NewRecorder()
	downloadFile(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("download: %d %s", w.Code, w.Body.String())
	}
	cd := w.Header().Get("Content-Disposition")
	if strings.Contains(cd, "\"evil\"\".txt\"") || strings.Contains(cd, "evil\".txt") {
		t.Fatalf("header carries the quote: %q", cd)
	}
	if !strings.Contains(cd, "evil.txt") {
		t.Fatalf("sanitized name missing: %q", cd)
	}
}
