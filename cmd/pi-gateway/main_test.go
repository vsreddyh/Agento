package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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

// Routing reads a leading reserved segment as "no profile", so a profile with one of
// those names would start, hold a process, and be unreachable from every request.
// Skipped at startup rather than served wrong.
//
// Driven from gateway.ReservedProfileNames rather than a local copy: discovery
// originally skipped only "v1" while routing reserved "v1" and "api", which is
// exactly how the two lists drift apart.
func TestDiscoverProfilesSkipsReservedNames(t *testing.T) {
	if len(gateway.ReservedProfileNames) < 2 {
		t.Fatalf("expected at least v1 and api reserved, got %v", gateway.ReservedProfileNames)
	}
	dir := t.TempDir()
	want := []string{"god", "story"}
	names := append(append([]string{}, want...), gateway.ReservedProfileNames...)
	for _, name := range names {
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
		if gateway.IsReservedProfileName(p) {
			t.Fatalf("reserved profile name %q was offered", p)
		}
	}
	if len(profiles) != len(want) {
		t.Fatalf("profiles = %v, want %v", profiles, want)
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

// PI_INVENTORY_TIMEOUT must be tunable like every other budget here. It was fixed at
// 30s in withDefaults with no way to change it, so a loaded host that needed longer
// had to rebuild the binary.
func TestInventoryTimeoutIsConfigurable(t *testing.T) {
	os.Unsetenv("PI_TEST_INVENTORY")
	if got := secondsEnv("PI_TEST_INVENTORY", 30); got.Seconds() != 30 {
		t.Fatalf("unset: got %v, want 30s", got)
	}
	t.Setenv("PI_TEST_INVENTORY", "90")
	if got := secondsEnv("PI_TEST_INVENTORY", 30); got.Seconds() != 90 {
		t.Fatalf("set to 90: got %v, want 90s", got)
	}
	// Garbage falls back rather than producing a zero budget that would fail every
	// inventory request instantly.
	t.Setenv("PI_TEST_INVENTORY", "abc")
	if got := secondsEnv("PI_TEST_INVENTORY", 30); got.Seconds() != 30 {
		t.Fatalf("garbage: got %v, want the 30s default", got)
	}
}

// Every documented budget must actually reach the field it configures. This is the
// assertion that was missing: exercising secondsEnv directly proved the parser works
// and said nothing about the wiring, so deleting the InventoryTimeout line from
// gatewayConfig left the suite green.
//
// Driven from the env var names, so a budget added to Config without a variable — or
// a variable parsed but never passed through — fails here.
func TestGatewayConfigPassesEveryBudgetThrough(t *testing.T) {
	for _, tc := range []struct {
		env   string
		value string
		want  time.Duration
		get   func(gateway.Config) time.Duration
	}{
		{"PI_TURN_TIMEOUT", "111", 111 * time.Second,
			func(c gateway.Config) time.Duration { return c.TurnTimeout }},
		{"PI_KEEPALIVE", "22", 22 * time.Second,
			func(c gateway.Config) time.Duration { return c.KeepaliveInterval }},
		{"PI_INVENTORY_TIMEOUT", "33", 33 * time.Second,
			func(c gateway.Config) time.Duration { return c.InventoryTimeout }},
	} {
		t.Run(tc.env, func(t *testing.T) {
			t.Setenv(tc.env, tc.value)
			cfg := gatewayConfig("pw", "/opt/pi", "god", func(string, ...any) {})
			if got := tc.get(cfg); got != tc.want {
				t.Fatalf("%s=%s did not reach the config: got %v, want %v",
					tc.env, tc.value, got, tc.want)
			}
		})
	}

	// Defaults, with nothing set.
	for _, k := range []string{"PI_TURN_TIMEOUT", "PI_KEEPALIVE", "PI_INVENTORY_TIMEOUT"} {
		os.Unsetenv(k)
	}
	cfg := gatewayConfig("pw", "/opt/pi", "god", func(string, ...any) {})
	for label, got := range map[string]time.Duration{
		"TurnTimeout":       cfg.TurnTimeout,
		"KeepaliveInterval": cfg.KeepaliveInterval,
		"InventoryTimeout":  cfg.InventoryTimeout,
	} {
		if got <= 0 {
			t.Errorf("%s defaulted to %v; every budget needs a usable value", label, got)
		}
	}

	// The non-timeout fields must not be lost in the extraction.
	if cfg.Password != "pw" || cfg.AgentDir != "/opt/pi" || cfg.DefaultProfile != "god" {
		t.Errorf("non-timeout fields not carried through: %+v", cfg)
	}
	if cfg.Logf == nil {
		t.Error("Logf was dropped by the extraction; a nil Logf would panic in New")
	}
}

// PI_KEEPALIVE=0 must still mean "disabled" through the extracted builder, not just
// through secondsEnvAllowDisabled in isolation.
func TestGatewayConfigKeepaliveZeroStaysDisabled(t *testing.T) {
	t.Setenv("PI_KEEPALIVE", "0")
	cfg := gatewayConfig("pw", "/opt/pi", "god", func(string, ...any) {})
	if cfg.KeepaliveInterval != gateway.KeepaliveDisabled {
		t.Fatalf("PI_KEEPALIVE=0 gave %v, want KeepaliveDisabled", cfg.KeepaliveInterval)
	}
}

// --skill is the only route to per-profile skills: Pi does not discover a bare
// skills/ directory in the working directory (measured — <agent-dir>/skills/ is
// found, <cwd>/skills/ is not). So the argument has to be added here, per profile.
func TestSkillArgsPointAtTheProfileSkillDir(t *testing.T) {
	profile := t.TempDir()
	skills := filepath.Join(profile, "skills")
	// A real SKILL.md, not just a directory: "has entries" is not "has skills", and a
	// bare subdirectory is deliberately not enough (see TestSkillArgsIgnoreStrayFiles).
	if err := os.MkdirAll(filepath.Join(skills, "some-skill"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "some-skill", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := skillArgs(profileSkillDir(profile))
	if len(got) != 2 || got[0] != "--skill" || got[1] != skills {
		t.Fatalf("skillArgs = %v, want [--skill %s]", got, skills)
	}
}

// A profile with no skills must still start: Pi errors on a --skill path that does not
// exist, so an unconditional argument would take out any skill-less profile.
func TestSkillArgsOmittedWhenNoSkills(t *testing.T) {
	profile := t.TempDir()
	if got := skillArgs(profileSkillDir(profile)); got != nil {
		t.Fatalf("no skills dir: got %v, want nil", got)
	}

	// Present but empty is the same thing.
	skills := filepath.Join(profile, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	if got := skillArgs(profileSkillDir(profile)); got != nil {
		t.Fatalf("empty skills dir: got %v, want nil", got)
	}
}

// profileSkillDir is the path convention, pinned so it cannot move without this
// failing: skills live at <profileDir>/skills, which is what --skill is pointed at.
func TestProfileSkillDirConvention(t *testing.T) {
	if got, want := profileSkillDir("/opt/pi/profiles/story"),
		"/opt/pi/profiles/story/skills"; got != want {
		t.Fatalf("profileSkillDir = %q, want %q", got, want)
	}
}

// A stray file in skills/ must NOT opt a profile into --skill. "Has entries" is not
// "has skills": a README or a .DS_Store would otherwise point Pi at a directory with
// no SKILL.md in it, and what Pi does with that is untested — so the question is not
// asked.
func TestSkillArgsIgnoreStrayFiles(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, dir string)
	}{
		{"a plain file in skills/", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a dotfile in skills/", func(t *testing.T, dir string) {
			if err := os.WriteFile(filepath.Join(dir, ".DS_Store"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
		}},
		{"a subdirectory with no SKILL.md", func(t *testing.T, dir string) {
			if err := os.MkdirAll(filepath.Join(dir, "not-a-skill"), 0o755); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			profile := t.TempDir()
			skills := filepath.Join(profile, "skills")
			if err := os.MkdirAll(skills, 0o755); err != nil {
				t.Fatal(err)
			}
			tc.setup(t, skills)
			if got := skillArgs(profileSkillDir(profile)); got != nil {
				t.Fatalf("got %v, want nil — no valid skill is present", got)
			}
		})
	}

	// And one real skill alongside the junk is still found.
	profile := t.TempDir()
	skills := filepath.Join(profile, "skills")
	if err := os.MkdirAll(filepath.Join(skills, "real"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "real", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "README.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := skillArgs(profileSkillDir(profile)); len(got) != 2 {
		t.Fatalf("got %v, want a --skill pair alongside the junk", got)
	}
}

// A skills directory that exists but cannot be read is a misconfiguration, and must
// be reported rather than silently running without skills — the failure mode being a
// profile that mysteriously lost its skills with nothing in the logs.
func TestSkillArgsReportsUnreadableDirectory(t *testing.T) {
	var logged strings.Builder
	saved := warnf
	warnf = func(format string, args ...any) { logged.WriteString(fmt.Sprintf(format, args...)) }
	defer func() { warnf = saved }()

	profile := t.TempDir()
	skills := filepath.Join(profile, "skills")
	if err := os.MkdirAll(skills, 0o755); err != nil {
		t.Fatal(err)
	}
	// Make the directory unreadable. Running as root defeats a permission bit, so
	// point at a path whose parent is a FILE instead: os.ReadDir then fails with
	// ENOTDIR, which is the same class of unexpected error and works as root.
	notADir := filepath.Join(profile, "skills-file")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := skillArgs(notADir); got != nil {
		t.Fatalf("got %v, want nil", got)
	}
	if !strings.Contains(logged.String(), "cannot read skills directory") {
		t.Fatalf("an unreadable skills dir was silent: %q", logged.String())
	}

	// An ABSENT directory is the expected case and must stay quiet.
	logged.Reset()
	if got := skillArgs(filepath.Join(profile, "nope")); got != nil {
		t.Fatalf("absent: got %v, want nil", got)
	}
	if logged.String() != "" {
		t.Fatalf("an absent skills dir was reported: %q", logged.String())
	}
}

// A subdirectory whose SKILL.md cannot be stat'd is a real skill this gateway would
// silently not load, and must be reported like the ReadDir failure above it.
//
// ENOTDIR is the reproducible trigger: SKILL.md is a symlink whose target runs
// through a regular file, so stat fails with ENOTDIR — which is NOT IsNotExist, so it
// reaches the reporting branch. A permission bit would be the natural choice and
// cannot be used, because these tests may run as root.
func TestSkillArgsReportsUnstattableSkill(t *testing.T) {
	var logged strings.Builder
	saved := warnf
	warnf = func(format string, args ...any) { logged.WriteString(fmt.Sprintf(format, args...)) }
	defer func() { warnf = saved }()

	// Prove the trigger is what this test assumes, rather than trusting it.
	probe := t.TempDir()
	if err := os.Symlink("/etc/hostname/nope", filepath.Join(probe, "SKILL.md")); err != nil {
		t.Skipf("cannot create the symlink this test needs: %v", err)
	}
	_, statErr := os.Stat(filepath.Join(probe, "SKILL.md"))
	if statErr == nil || os.IsNotExist(statErr) {
		t.Skipf("expected a non-IsNotExist stat error, got %v", statErr)
	}

	profile := t.TempDir()
	skills := filepath.Join(profile, "skills")
	if err := os.MkdirAll(filepath.Join(skills, "broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/etc/hostname/nope", filepath.Join(skills, "broken", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	if got := skillArgs(profileSkillDir(profile)); got != nil {
		t.Fatalf("got %v, want nil — the only skill is unusable", got)
	}
	if !strings.Contains(logged.String(), "cannot stat") {
		t.Fatalf("an unstattable skill was silent: %q", logged.String())
	}
	if !strings.Contains(logged.String(), "broken") {
		t.Fatalf("the offending skill was not named: %q", logged.String())
	}
}

// The happy path with nothing to report: a healthy skill, no warning.
//
// Named for what it does rather than what I first intended. It was going to prove a
// healthy skill is still found ALONGSIDE an unusable one, but ReadDir sorts by name and
// "a-good" returns before "b-broken" is ever visited — so that would not have been
// tested at all. The warning path is covered by TestSkillArgsReportsUnstattableSkill.
func TestSkillArgsFindsHealthySkillWithoutWarning(t *testing.T) {
	var logged strings.Builder
	saved := warnf
	warnf = func(format string, args ...any) { logged.WriteString(fmt.Sprintf(format, args...)) }
	defer func() { warnf = saved }()

	profile := t.TempDir()
	skills := filepath.Join(profile, "skills")
	// "a-good" sorts before "b-broken", so the healthy skill returns before the broken
	// one is visited — which means this cannot also prove the warning fires. Kept as the
	// positive case only; the warning is covered by the test above.
	if err := os.MkdirAll(filepath.Join(skills, "a-good"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "a-good", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got := skillArgs(profileSkillDir(profile))
	if len(got) != 2 || got[0] != "--skill" {
		t.Fatalf("got %v, want a --skill pair", got)
	}
	if logged.String() != "" {
		t.Fatalf("nothing should have been reported: %q", logged.String())
	}
}

// A broken skill sorting AFTER a healthy one must still be reported. ReadDir sorts by
// name, so an early return on the first valid SKILL.md meant this profile loaded fine
// and the operator was never told part of it would not be there — the warning only
// fired when the broken skill happened to sort first, which is luck, not behaviour.
func TestSkillArgsReportsBrokenSkillAfterAHealthyOne(t *testing.T) {
	var logged strings.Builder
	saved := warnf
	warnf = func(format string, args ...any) { logged.WriteString(fmt.Sprintf(format, args...)) }
	defer func() { warnf = saved }()

	profile := t.TempDir()
	skills := filepath.Join(profile, "skills")

	// "a-good" sorts first and is valid.
	if err := os.MkdirAll(filepath.Join(skills, "a-good"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(skills, "a-good", "SKILL.md"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	// "b-broken" sorts second, so it is only reached if the loop does not return early.
	if err := os.MkdirAll(filepath.Join(skills, "b-broken"), 0o755); err != nil {
		t.Fatal(err)
	}
	probe := t.TempDir()
	if err := os.Symlink("/etc/hostname/nope", filepath.Join(probe, "SKILL.md")); err != nil {
		t.Skipf("cannot create the symlink this test needs: %v", err)
	}
	if _, err := os.Stat(filepath.Join(probe, "SKILL.md")); err == nil || os.IsNotExist(err) {
		t.Skipf("expected a non-IsNotExist stat error, got %v", err)
	}
	if err := os.Symlink("/etc/hostname/nope", filepath.Join(skills, "b-broken", "SKILL.md")); err != nil {
		t.Fatal(err)
	}

	got := skillArgs(profileSkillDir(profile))
	if len(got) != 2 || got[0] != "--skill" {
		t.Fatalf("got %v, want a --skill pair from the healthy skill", got)
	}
	if !strings.Contains(logged.String(), "b-broken") {
		t.Fatalf("the broken skill sorting after the healthy one was not reported: %q",
			logged.String())
	}
}
