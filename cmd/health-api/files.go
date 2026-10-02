package main

// VPS exports browser: list + download (#171).

import (
	"go.mongodb.org/mongo-driver/bson"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"strings"
	"time"
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
func registerFiles(mux *http.ServeMux) {
	mux.HandleFunc("/api/files", listFiles)
	mux.HandleFunc("/api/files/download", downloadFile)
}
