// Package crashlog records fatal-crash files with a 5-day TTL (#290).
//
// Crash logs only — handler panics and dead agents, not every error.
// Client and server, same TTL everywhere. Saved on the device (or the
// bind-mounted agent dir, which survives container restarts); the user
// retrieves the files by hand.
//
// Redaction: crash text must never carry credentials. WriteCrash redacts
// the given secrets plus any Authorization bearer header before writing;
// callers pass PASSWORD and MONGODB_URI (which carries credentials).
package crashlog

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

// TTL is how long a crash file lives: files older than this are deleted
// by Prune, which runs at startup and after each write.
const TTL = 5 * 24 * time.Hour

const timeFormat = "20060102-150405"

// bearerRe matches an Authorization bearer header value. The token shape
// requires 8+ token characters so prose like "Bearer token" is left alone.
var bearerRe = regexp.MustCompile(`(?i)(Authorization:\s*Bearer\s+)[A-Za-z0-9\-._~+/=]{8,}`)

// FileName is crash-<yyyyMMdd-HHmmss>.log, the issue's file shape.
func FileName(now time.Time) string {
	return "crash-" + now.Format(timeFormat) + ".log"
}

// Redact replaces every secret and every bearer header value with ***.
// Empty secrets are skipped: replacing "" would insert *** everywhere.
func Redact(text string, secrets ...string) string {
	for _, s := range secrets {
		if s != "" {
			text = strings.ReplaceAll(text, s, "***")
		}
	}
	return bearerRe.ReplaceAllString(text, "${1}***")
}

// WriteCrash writes detail (already assembled by the caller) to a crash
// file under dir, redacting secrets first. Same-second collisions get a
// numeric suffix so no crash overwrites another. Files are 0600: a stack
// trace names internals, and world-readable is not the default anything
// diagnostic should choose. Returns the file path.
func WriteCrash(dir, detail string, secrets ...string) (string, error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	text := Redact(detail, secrets...)
	now := time.Now()
	for i := 0; ; i++ {
		name := FileName(now)
		if i > 0 {
			name = fmt.Sprintf("crash-%s-%d.log", now.Format(timeFormat), i)
		}
		path := filepath.Join(dir, name)
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if err != nil {
			if os.IsExist(err) {
				continue
			}
			return "", err
		}
		if _, err := f.WriteString(text); err != nil {
			_ = f.Close()
			return "", err
		}
		if err := f.Close(); err != nil {
			return "", err
		}
		return path, nil
	}
}

// Prune deletes crash-*.log files under dir older than TTL. Missing dir
// is not an error (nothing recorded yet). Other files are left alone.
func Prune(dir string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	now := time.Now()
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "crash-") || !strings.HasSuffix(e.Name(), ".log") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.Sub(info.ModTime()) > TTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}
