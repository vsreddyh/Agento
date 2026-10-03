package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agento/internal/gateway"
)

// discoverProfiles is the gate between "a directory exists" and "Pi loads this
// profile's personality", so its edge cases matter: a stray folder must not
// become a profile, and a real profile must never be skipped.
func TestDiscoverProfilesRequiresAgentsFile(t *testing.T) {
	dir := t.TempDir()

	// A proper profile: a directory with AGENTS.md.
	if err := os.MkdirAll(filepath.Join(dir, "god", "skills"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "god", "AGENTS.md"), []byte("# god\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A directory with no AGENTS.md is not a profile.
	if err := os.MkdirAll(filepath.Join(dir, "not-a-profile"), 0o755); err != nil {
		t.Fatal(err)
	}

	// A dotfile directory is skipped even with an AGENTS.md.
	if err := os.MkdirAll(filepath.Join(dir, ".cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, ".cache", "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// A loose AGENTS.md at the top level is not a profile either.
	if err := os.WriteFile(filepath.Join(dir, "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	profiles, err := discoverProfiles(dir)
	if err != nil {
		t.Fatalf("discoverProfiles: %v", err)
	}
	if len(profiles) != 1 || profiles[0] != "god" {
		t.Fatalf("profiles = %v, want [god]", profiles)
	}
}

func TestDiscoverProfilesMissingDirectory(t *testing.T) {
	_, err := discoverProfiles(filepath.Join(t.TempDir(), "absent"))
	if err == nil {
		t.Fatal("a missing profile directory should be an error, not an empty list")
	}
}

func TestEnvOrFallsBack(t *testing.T) {
	t.Setenv("PI_TEST_ENV_OR", "  value  ")
	if got := envOr("PI_TEST_ENV_OR", "fallback"); got != "value" {
		t.Fatalf("envOr = %q, want %q (whitespace should be trimmed)", got, "value")
	}

	t.Setenv("PI_TEST_ENV_OR", "")
	if got := envOr("PI_TEST_ENV_OR", "fallback"); got != "fallback" {
		t.Fatalf("envOr = %q, want the fallback", got)
	}

	os.Unsetenv("PI_TEST_ENV_UNSET")
	if got := envOr("PI_TEST_ENV_UNSET", "fallback"); got != "fallback" {
		t.Fatalf("envOr = %q, want the fallback", got)
	}
}

func TestSecondsEnv(t *testing.T) {
	// Absent, malformed, zero and negative all fall back rather than producing a
	// zero or negative duration that would disable the timeout entirely.
	os.Unsetenv("PI_TEST_SECONDS")
	for _, v := range []string{"", "abc", "0", "-5"} {
		t.Setenv("PI_TEST_SECONDS", v)
		if got := secondsEnv("PI_TEST_SECONDS", 30); got.Seconds() != 30 {
			t.Fatalf("secondsEnv(%q) = %v, want 30s", v, got)
		}
	}

	t.Setenv("PI_TEST_SECONDS", "90")
	if got := secondsEnv("PI_TEST_SECONDS", 30); got.Seconds() != 90 {
		t.Fatalf("secondsEnv = %v, want 90s", got)
	}
}

// The gateway authenticates to the app; the agent never needs the credential and
// has no business holding one. Anything the agent runs — including tool output —
// can read its environment.
func TestPasswordIsStrippedFromChildEnv(t *testing.T) {
	base := []string{"PATH=/bin", "PASSWORD=secret", "OPENCODE_API_KEY=key"}

	stripped := withoutEnv(base, "PASSWORD")
	for _, e := range stripped {
		if strings.HasPrefix(strings.ToUpper(e), "PASSWORD=") {
			t.Fatalf("PASSWORD survived into the child env: %v", stripped)
		}
	}

	// Everything else is preserved — stripping must not cost the child its PATH,
	// which is the mistake that makes exec fail in an unrelated-looking way.
	joined := strings.Join(stripped, "|")
	if !strings.Contains(joined, "PATH=/bin") {
		t.Fatalf("stripping removed PATH: %v", stripped)
	}
	if !strings.Contains(joined, "OPENCODE_API_KEY=key") {
		t.Fatalf("stripping removed an unrelated variable: %v", stripped)
	}
}

// Case must not matter: env var names are case-sensitive on Linux but not on
// every platform, and a missed PASSWORD on Windows would be a real leak.
func TestWithoutEnvIsCaseInsensitive(t *testing.T) {
	for _, name := range []string{"PASSWORD", "password", "Password"} {
		got := withoutEnv([]string{name + "=secret", "PATH=/bin"}, "PASSWORD")
		if strings.Contains(strings.Join(got, "|"), "secret") {
			t.Fatalf("%s survived: %v", name, got)
		}
		if len(got) != 1 {
			t.Fatalf("%s: got %v, want only PATH", name, got)
		}
	}
}

// Routing reads a leading "v1" segment as "no profile", so a profile with that
// name would be unreachable. It is skipped at startup rather than served wrong.
func TestDiscoverProfilesSkipsReservedName(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"god", "story", reservedProfileName} {
		if err := os.MkdirAll(filepath.Join(dir, name), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name, "AGENTS.md"), []byte("x"), 0o644); err != nil {
			t.Fatal(err)
		}
	}

	profiles, err := discoverProfiles(dir)
	if err != nil {
		t.Fatalf("discoverProfiles: %v", err)
	}
	for _, p := range profiles {
		if p == reservedProfileName {
			t.Fatalf("reserved profile name %q was offered", p)
		}
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles = %v, want god and story", profiles)
	}
}

// captureStderr runs fn with os.Stderr redirected and returns what it wrote.
// The output under test is a single short line, well under the pipe buffer, so
// reading after the writer closes cannot deadlock.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	saved := os.Stderr
	os.Stderr = w
	fn()
	os.Stderr = saved
	_ = w.Close()

	var sb strings.Builder
	buf := make([]byte, 4096)
	for {
		n, err := r.Read(buf)
		sb.Write(buf[:n])
		if err != nil {
			break
		}
	}
	_ = r.Close()
	return sb.String()
}

// A directory with no AGENTS.md is not a profile, and that is the common case —
// it should pass without a word. Anything else going wrong means the profile
// probably was meant to be served and could not be, and silently dropping it
// surfaces later as "the profile is missing" after a deploy that quietly lost it.
//
// A self-referencing symlink triggers ELOOP, which is a real stat failure the
// process running as root still cannot see through — unlike a permission bit.
func TestDiscoverProfilesDistinguishesMissingFromUnreadable(t *testing.T) {
	dir := t.TempDir()

	if err := os.Mkdir(filepath.Join(dir, "plain"), 0o755); err != nil {
		t.Fatalf("mkdir plain: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "good"), 0o755); err != nil {
		t.Fatalf("mkdir good: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "good", "AGENTS.md"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write AGENTS.md: %v", err)
	}
	if err := os.Mkdir(filepath.Join(dir, "loop"), 0o755); err != nil {
		t.Fatalf("mkdir loop: %v", err)
	}
	if err := os.Symlink("AGENTS.md", filepath.Join(dir, "loop", "AGENTS.md")); err != nil {
		t.Fatalf("symlink: %v", err)
	}

	var profiles []string
	var err error
	out := captureStderr(t, func() { profiles, err = discoverProfiles(dir) })
	if err != nil {
		t.Fatalf("discoverProfiles: %v", err)
	}

	if len(profiles) != 1 || profiles[0] != "good" {
		t.Fatalf("profiles = %v, want [good]", profiles)
	}
	if !strings.Contains(out, "loop") || !strings.Contains(out, "cannot read AGENTS.md") {
		t.Fatalf("unreadable AGENTS.md was skipped silently; stderr = %q", out)
	}
	// The ordinary case must stay quiet.
	if strings.Contains(out, "plain") {
		t.Fatalf("a directory with no AGENTS.md should not be reported; stderr = %q", out)
	}
}

// PI_KEEPALIVE=0 is documented as disabling the keepalive comments, and it has to
// actually do that. It used to warn and fall back to the default, so the one value
// an operator reaches for when disabling was the one value that did nothing.
func TestKeepaliveZeroDisables(t *testing.T) {
	os.Unsetenv("PI_TEST_KEEPALIVE")
	if got := secondsEnvAllowDisabled("PI_TEST_KEEPALIVE", 20); got.Seconds() != 20 {
		t.Fatalf("unset: got %v, want 20s", got)
	}

	t.Setenv("PI_TEST_KEEPALIVE", "0")
	if got := secondsEnvAllowDisabled("PI_TEST_KEEPALIVE", 20); got != gateway.KeepaliveDisabled {
		t.Fatalf("explicit 0: got %v, want KeepaliveDisabled (%v)", got, gateway.KeepaliveDisabled)
	}

	t.Setenv("PI_TEST_KEEPALIVE", "5")
	if got := secondsEnvAllowDisabled("PI_TEST_KEEPALIVE", 20); got.Seconds() != 5 {
		t.Fatalf("5: got %v, want 5s", got)
	}

	// A negative is a mistake, not a request to disable: "off every -1 seconds"
	// is not a thing. It warns and uses the default rather than silently disabling.
	for _, v := range []string{"-1", "abc"} {
		t.Setenv("PI_TEST_KEEPALIVE", v)
		if got := secondsEnvAllowDisabled("PI_TEST_KEEPALIVE", 20); got.Seconds() != 20 {
			t.Fatalf("%q: got %v, want the 20s default", v, got)
		}
	}
}
