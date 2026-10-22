package main

// VPS exports browser: list + download + upload (#171, #340).

import (
	"go.mongodb.org/mongo-driver/bson"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
)

const (
	// maxUploadBytes caps one uploaded file at 25 MiB. Past it the server
	// refuses with a named 413 (file_too_large) rather than a silent
	// failure or an opaque 500: the phone picks with no size knowledge, so
	// the refusal must say the limit. 25 MiB holds receipt photos and PDFs
	// with headroom while keeping one phone-driven write bounded.
	maxUploadBytes = 25 << 20
	// maxNameBytes caps a file name. 255 matches NAME_MAX on the VPS
	// filesystem; anything longer cannot be stored anyway.
	maxNameBytes = 255
	// uploadField is the multipart field carrying the file bytes.
	uploadField = "file"
	// uploadTempPrefix is the staging pattern for in-flight uploads (see
	// uploadFile) and the filter listFiles applies so partials never list.
	uploadTempPrefix = ".upload-"
)

func exportsRoot() string {
	if v := strings.TrimSpace(os.Getenv("EXPORTS_DIR")); v != "" {
		return v
	}
	return "/exports"
}

// resolveExport cleans a user-supplied relative path and confines it inside
// the exports root (rejects absolute paths and .. escapes).
func resolveExport(rel string) (string, error) {
	rel = strings.TrimSpace(rel)
	if rel == "" {
		return exportsRoot(), nil
	}
	// Reject escapes before cleaning (Clean would normalize ".." away).
	if path.IsAbs(rel) || rel == ".." || strings.HasPrefix(rel, "../") ||
		strings.Contains(rel, "/../") || strings.HasSuffix(rel, "/..") {
		return "", errBadPath
	}
	full := filepath.Join(exportsRoot(), filepath.FromSlash(path.Clean("/"+rel)))
	root, err := filepath.EvalSymlinks(exportsRoot())
	if err != nil {
		return "", err
	}
	// Allow missing leaf (parent must still resolve inside root).
	target := full
	if _, err := os.Lstat(target); err != nil {
		target = filepath.Dir(target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return "", err
	}
	if resolved != root && !strings.HasPrefix(resolved, root+string(os.PathSeparator)) {
		return "", errBadPath
	}
	return full, nil
}

var errBadPath = errText("invalid path")

type errText string

func (e errText) Error() string { return string(e) }

func listFiles(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	full, err := resolveExport(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, bson.M{"detail": "invalid path"})
		return
	}
	entries, err := os.ReadDir(full)
	if err != nil {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
		return
	}
	rel, _ := filepath.Rel(exportsRoot(), full)
	dirs := []bson.M{}
	files := []bson.M{}
	for _, e := range entries {
		// Staging files from an in-flight upload (CreateTemp ".upload-*"
		// in this same dir, required for an atomic rename) never list: a
		// concurrent list in the upload window would otherwise show a
		// partial file that vanishes or renames a moment later.
		if strings.HasPrefix(e.Name(), uploadTempPrefix) {
			continue
		}
		if e.IsDir() {
			dirs = append(dirs, bson.M{"name": e.Name()})
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		files = append(files, bson.M{
			"name":  e.Name(),
			"size":  info.Size(),
			"mtime": info.ModTime().UTC().Format(time.RFC3339),
		})
	}
	if dirs == nil {
		dirs = []bson.M{}
	}
	if files == nil {
		files = []bson.M{}
	}
	writeJSON(w, http.StatusOK, bson.M{"path": filepath.ToSlash(rel), "dirs": dirs, "files": files})
}

func downloadFile(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	full, err := resolveExport(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, bson.M{"detail": "invalid path"})
		return
	}
	info, err := os.Stat(full)
	if err != nil || info.IsDir() {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "not found"})
		return
	}
	w.Header().Set("Content-Disposition", "attachment; filename=\""+info.Name()+"\"")
	http.ServeFile(w, r, full)
}

// registerFiles wires this domain's routes. One function per file so main stays a
// list of domains, not a list of paths (#171).
//
// Upload policy, decided and documented here (#340):
//   - Size cap: maxUploadBytes per file; past it the upload refuses with a
//     named 413 (file_too_large), never a silent failure or opaque 500/413.
//     The whole request body is capped before multipart parsing (cap + 1 MiB
//     framing slack), so a hostile body is refused during the parse, not
//     after buffering it.
//   - Overwrite policy: refuse. An existing name answers 409 (file_exists);
//     the server never silently replaces VPS data on a phone picker's
//     collision-prone names (IMG_001.jpg). Rename on device and retry.
//   - Confinement: the target dir and the file name both go through
//     resolveExport's rules — absolute paths and .. escapes are rejected,
//     and the write is symlink-checked into the exports root. Names are
//     bare base names: quotes and ASCII controls are rejected because the
//     name is interpolated into the download's Content-Disposition header.
//   - Staging: the write lands as a same-dir temp file (atomic rename) with
//     the uploadTempPrefix, which listFiles filters so partials never list.
func registerFiles(mux *http.ServeMux) {
	mux.HandleFunc("/api/files", listFiles)
	mux.HandleFunc("/api/files/download", downloadFile)
	mux.HandleFunc("/api/files/upload", uploadFile)
}

// cleanFileName validates a user-supplied file name: a bare base name, no
// separators, no dot-dots, no NUL, bounded length. The name is never joined
// before this passes, so "a/b" cannot escape the target dir even though
// resolveExport would also catch the join.
//
// Quotes and ASCII controls are rejected too: the name is interpolated into
// the download's Content-Disposition header, and a `"` or CR/LF in it would
// break or inject that header on every later download of the file.
func cleanFileName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" || name == "." || name == ".." {
		return "", errBadName
	}
	if strings.ContainsRune(name, 0) || strings.ContainsAny(name, `/\`) {
		return "", errBadName
	}
	for _, r := range name {
		if r == '"' || r < 0x20 || r == 0x7F {
			return "", errBadName
		}
	}
	// The staging prefix is reserved: a file literally named .upload-*
	// would store fine but never list (listFiles filters the prefix), with
	// no delete endpoint to reclaim it.
	if strings.HasPrefix(name, uploadTempPrefix) {
		return "", errBadName
	}
	if len(name) > maxNameBytes {
		return "", errBadName
	}
	return name, nil
}

var errBadName = errText("invalid_name: pick a plain file name without folders")

// uploadFile handles POST /api/files/upload?path=<dir> with the file bytes
// as multipart field "file" and the file name as ?name=. The dir is the
// Files tab's current folder and must already exist; the write is atomic
// (temp file + rename) and refused — never replaced — when the name exists.
func uploadFile(w http.ResponseWriter, r *http.Request) {
	if code, detail := authorize(r); code != 0 {
		writeJSON(w, code, bson.M{"detail": detail})
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		writeJSON(w, http.StatusMethodNotAllowed, bson.M{"detail": "POST only"})
		return
	}
	dir, err := resolveExport(r.URL.Query().Get("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, bson.M{"detail": "invalid path"})
		return
	}
	if info, err := os.Stat(dir); err != nil || !info.IsDir() {
		writeJSON(w, http.StatusNotFound, bson.M{"detail": "folder not found"})
		return
	}
	name, err := cleanFileName(r.URL.Query().Get("name"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, bson.M{"detail": err.Error()})
		return
	}
	// Bound the whole request before multipart parsing: without this a
	// hostile body far past the cap is buffered (memory + temp disk) only
	// to be answered 413 afterwards. The slack covers multipart framing
	// around a max-size file; anything past it is too large by
	// construction, whatever the framing claims.
	r.Body = http.MaxBytesReader(w, r.Body, maxUploadBytes+(1<<20))
	part, _, err := r.FormFile(uploadField)
	if err != nil {
		if tooLarge(err) {
			writeJSON(w, http.StatusRequestEntityTooLarge, bson.M{"detail": "file_too_large: limit is 25 MiB — shrink the file and retry"})
			return
		}
		writeJSON(w, http.StatusBadRequest, bson.M{"detail": "file_missing: send the file as multipart field \"file\""})
		return
	}
	defer part.Close()
	target := filepath.Join(dir, name)
	if _, err := os.Lstat(target); err == nil {
		writeJSON(w, http.StatusConflict, bson.M{"detail": "file_exists: rename on device and retry — the server never replaces an existing file"})
		return
	}
	// Re-check confinement on the joined target: the name passed validation,
	// but the join is verified rather than trusted, the same belt-and-braces
	// resolveExport applies to reads.
	if _, err := resolveExport(joinRel(r.URL.Query().Get("path"), name)); err != nil {
		writeJSON(w, http.StatusBadRequest, bson.M{"detail": "invalid path"})
		return
	}
	tmp, err := os.CreateTemp(dir, uploadTempPrefix+"*")
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "cannot stage upload"})
		return
	}
	tmpName := tmp.Name()
	// One past the cap, so a file exactly at the cap still fits while
	// anything over is detected without reading it all into memory.
	n, err := io.Copy(tmp, io.LimitReader(part, maxUploadBytes+1))
	_ = tmp.Close()
	if err != nil {
		_ = os.Remove(tmpName)
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "cannot stage upload"})
		return
	}
	if n > maxUploadBytes {
		_ = os.Remove(tmpName)
		writeJSON(w, http.StatusRequestEntityTooLarge, bson.M{"detail": "file_too_large: limit is 25 MiB — shrink the file and retry"})
		return
	}
	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "cannot stage upload"})
		return
	}
	if err := os.Rename(tmpName, target); err != nil {
		_ = os.Remove(tmpName)
		// A racing upload won the name between the Lstat and the rename:
		// still a conflict, not a 500.
		if _, statErr := os.Lstat(target); statErr == nil {
			writeJSON(w, http.StatusConflict, bson.M{"detail": "file_exists: rename on device and retry — the server never replaces an existing file"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "cannot store upload"})
		return
	}
	info, err := os.Stat(target)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, bson.M{"detail": "cannot store upload"})
		return
	}
	rel, _ := filepath.Rel(exportsRoot(), target)
	writeJSON(w, http.StatusOK, bson.M{
		"name":  info.Name(),
		"size":  info.Size(),
		"mtime": info.ModTime().UTC().Format(time.RFC3339),
		"path":  filepath.ToSlash(filepath.Dir(rel)),
	})
}

// tooLarge reports whether err is the MaxBytesReader bound firing.
func tooLarge(err error) bool {
	return err != nil && strings.Contains(err.Error(), "request body too large")
}

// joinRel joins a ?path= dir and a validated file name back into the
// slash-separated relative form resolveExport expects.
func joinRel(dir, name string) string {
	dir = strings.Trim(strings.TrimSpace(dir), "/")
	if dir == "" {
		return name
	}
	return dir + "/" + name
}
