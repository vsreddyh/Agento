// Package skills holds checks over the Pi agent's skill files.
//
// It exists because of three defects that shared one shape: a SKILL.md that was
// correct when written and wrong later, with nothing failing. A skill that points at
// a moved path, tells the agent to run a binary its container does not have, or is
// missing its final newline is invisible in review and invisible at runtime — the
// agent simply cannot do what the file says.
//
// Every rule here is mechanical and deterministic, because a check that needs a
// container, a network or a judgement call is a check that gets skipped. The
// container-specific facts (which binaries exist in the pi image, which paths are
// bind-mounted) are asserted as constants with the evidence recorded beside them, so
// a change to the image shows up as a failing test rather than as stale prose.
package skills

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// repoRoot is two levels up from internal/skills.
const repoRoot = "../.."

// Container facts, verified by running the checks in the live pi container:
//
//	cwd                     /workspace
//	mounts                  ../pi -> /opt/pi, ../workspace -> /workspace, 3 read-only SSH files
//	podman / docker / compose  absent
//	repo's docker-compose.yml  absent
//	.env                    absent (compose interpolation instead)
//
// In-containerMounts are the paths a SKILL.md may reference as existing at runtime.
// Anything else repo-relative is a host path or a bug.
// Ordered longest-prefix-first so `/opt/pi` is matched before a shorter root that
// would also be a prefix of it. The repo-relative equivalent is what makes the check
// useful twice over: the path is validated against the mounts, and the file it names
// is validated against the tree.
var inContainerMounts = []struct {
	inContainer string
	inRepo      string
}{
	{"/opt/pi", "pi"},
	{"/workspace", "workspace"},
}

// Enforced rather than documented: the prefix match below takes the first hit, so a
// mount added later that is a prefix of an existing one would shadow it. Sorting by
// descending length at init means the list cannot be written in the wrong order.
func init() {
	sort.SliceStable(inContainerMounts, func(i, j int) bool {
		return len(inContainerMounts[i].inContainer) > len(inContainerMounts[j].inContainer)
	})
}

// hostOnlyTools are binaries the pi image does not have, as word-boundary patterns.
// A SKILL.md that mentions one is writing instructions for the human, and must say so
// — otherwise the agent tries to run it and reports "command not found" as a stack
// fault.
//
// Word boundaries, not substrings: `docker-compose` contains `docker`, and so does
// any prose word that happens to embed it. A substring match here produces a failure
// whose message names a tool the file never mentions, which is worse than no check.
// A display name alongside each pattern: tool.String() leaks the regex source, so a
// failure would read "mentions [\bpodman\b]" and the reader would have to decode it.
// `(?i)` on every pattern, because prose capitalises: the heading in this very
// skill's file is "# Podman Management", and a case-sensitive scan lets any
// capitalised mention walk straight past the rule.
var hostOnlyTools = []struct {
	pattern *regexp.Regexp
	display string
}{
	{regexp.MustCompile(`(?i)\bpodman-compose\b`), "podman-compose"},
	{regexp.MustCompile(`(?i)\bpodman\b`), "podman"},
	{regexp.MustCompile(`(?i)\bdocker\b`), "docker"},
	{regexp.MustCompile(`(?i)\bcompose\b`), "compose"},
	{regexp.MustCompile(`(?i)\bsystemctl\b`), "systemctl"},
}

// hostSplitMarkers are the phrases that discharge the hostOnlyTools rule. More than
// one is accepted because the honest phrasing varies; what matters is that the file
// tells its reader which side of the container boundary it is on.
// Word-boundary patterns, NOT substrings. `strings.Contains(lowered, "host only")`
// also matches the tail of "ghost only", so prose about something else silently
// discharged the rule — a check that can be satisfied by an unrelated word is not a
// check. `[ -]` covers the spaced and hyphenated spellings in one pattern, since prose
// writes "host only" and a heading writes "Host-only:", and a list covering only one
// leaves an author using the other wondering why it was ignored.
var hostSplitMarkers = []*regexp.Regexp{
	regexp.MustCompile(`(?i)\bnot on the host\b`),
	regexp.MustCompile(`(?i)\bfor the human\b`),
	regexp.MustCompile(`(?i)\bhost[ -]only\b`),
}

var (
	// Matched inside the extracted frontmatter block, never against the whole body:
	// a `name:` or `description:` mentioned in prose is not frontmatter, and a check
	// that cannot tell the difference is a check that can be satisfied by a sentence.
	frontmatterName = regexp.MustCompile(`(?m)^name:\s*\S+`)
	frontmatterDesc = regexp.MustCompile(`(?m)^description:\s*\S+`)

	// A repo-relative path in backticks, e.g. `pi/skills/task-manager/SKILL.md`.
	// An optional `./` prefix is accepted, because `./scripts/hermes.sh` is how the
	// docs write it and skipping it would be a silent false negative. ANY extension,
	// not an allowlist: an earlier list of `md|go|sh|…` let `Main_Resume.tex` and every
	// `.py` reference past unexamined, and the resumes profile compiles `.tex` for a
	// living. Stat decides what exists; this only decides what is worth asking about.
	// A match with no `/` in it is rejected by the consumer rather than here:
	// `AGENTS.md` alone is a bare filename, and resolving it against the repo root
	// would be wrong.
	backtickedPath = regexp.MustCompile("`((?:\\./|\\.\\./)*[A-Za-z0-9_][A-Za-z0-9_./-]*\\.[A-Za-z0-9]+)`")

	// Absolute paths that only make sense inside a container, e.g.
	// /opt/pi/profiles/god/AGENTS.md. Derived from inContainerMounts rather than written
	// as a literal, so adding a mount cannot leave the regex validating less than the
	// list claims — the two drifting is the bug this rule exists to catch elsewhere.

	// A bare `skills/<name>/SKILL.md`: the repo-root tree nothing discovers. Matching
	// `pi/skills/...` is fine, so the leading boundary is required.
	// `./skills/foo/SKILL.md` needs its own alternative, not a wider boundary class: the
	// character before `skills` there is `/`, which the class excludes on purpose (it
	// stops `pi/skills/…` matching). So the dot-slash is matched explicitly — and it is
	// the spelling a reader is most likely to copy out of a shell command.
	bareSkillsPath = regexp.MustCompile("(?:^|[^/A-Za-z0-9_.-]|\\./)(skills/[A-Za-z0-9_-]+/SKILL\\.md)")
)

// skillFiles returns every SKILL.md Pi can load: the agent dir (shared) and each
// profile's own (passed with --skill).
func skillFiles(t *testing.T) []string {
	t.Helper()
	var found []string
	for _, pattern := range []string{
		filepath.Join(repoRoot, "pi", "skills", "*", "SKILL.md"),
		filepath.Join(repoRoot, "pi", "profiles", "*", "skills", "*", "SKILL.md"),
	} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %s: %v", pattern, err)
		}
		found = append(found, matches...)
	}
	if len(found) == 0 {
		t.Fatal("no SKILL.md files found — the globs are wrong, or every skill was deleted")
	}
	return found
}

func read(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// TestSkillFrontmatterIsComplete: Pi keys the Skills screen off name+description, and
// a skill missing either is listed as blank rather than refused.
func TestSkillFrontmatterIsComplete(t *testing.T) {
	for _, path := range skillFiles(t) {
		body := read(t, path)
		block, ok := frontmatterOf(body)
		if !ok {
			t.Errorf("%s: does not open with a closed `---` frontmatter block", path)
			continue
		}
		if !frontmatterName.MatchString(block) {
			t.Errorf("%s: frontmatter has no `name:`", path)
		}
		if !frontmatterDesc.MatchString(block) {
			t.Errorf("%s: frontmatter has no `description:`", path)
		}
	}
}

// TestSkillPathsResolve: every repo-relative path a skill names must exist. This is
// the check that would have caught a skill left pointing at a moved tree.
func TestSkillPathsResolve(t *testing.T) {
	for _, path := range skillFiles(t) {
		// Frontmatter is metadata in every check, not just the host-tooling one: a
		// `description:` naming a backticked path is describing the skill, not telling the
		// agent to read it. One rule, applied in all three tests, is easier to hold onto
		// than a rule that applies "where it matters".
		body := instructionBody(read(t, path))
		for _, m := range backtickedPath.FindAllStringSubmatch(body, -1) {
			rel := m[1]
			if !strings.Contains(rel, "/") {
				continue // bare filename: not a path, so not resolvable as one
			}
			// A parent-relative path (`../shared/foo.md`) is written relative to the file
			// that mentions it, so it resolves against the SKILL's directory — not the repo
			// root, which would both mis-resolve a legitimate reference and let `../../etc/x`
			// stat something outside the checkout entirely.
			if hasTraversalSegment(rel) {
				// Base is the skill's own directory, because that is what the author was
				// looking at. The BOUNDARY is the repo, not the skill: `../podman-management/…`
				// is a legitimate reference to a sibling, and refusing it would push authors
				// towards longer paths that mean the same thing. What must be refused is a
				// walk out of the checkout — `../../../../etc/passwd` resolves to something
				// real on a developer machine and to nothing in CI.
				dir := filepath.Dir(path)
				target := filepath.Clean(filepath.Join(dir, rel))
				// filepath.Rel rather than HasPrefix on absolute paths: it answers "is target
				// inside root" directly, needs no per-match Abs, and cannot be fooled by a
				// prefix that is not a path boundary (`/repo-evil` vs `/repo`).
				if inside, errRel := filepath.Rel(repoRoot, target); errRel != nil ||
					inside == ".." || strings.HasPrefix(inside, ".."+string(os.PathSeparator)) {
					t.Errorf("%s: references `%s`, which resolves outside the checkout (%s) — "+
						"a skill should not reach out of the repo", path, rel, target)
					continue
				}
				if _, err := os.Stat(target); err != nil {
					t.Errorf("%s: references `%s`, which does not exist (%v)", path, rel, err)
				}
				continue
			}
			// The slash-less spelling of a container path (`opt/pi/skills/x.md`) is
			// checked HERE, against the same mount list the leading-slash form uses in
			// TestSkillContainerPathsAreMounted. It was previously skipped as "the other
			// test's job" — and it is not, because containerPathRe requires a leading `/`,
			// so the form passed both tests unchecked. Better to check a path twice than to
			// skip it once.
			// The slash-less form drops the leading "/", so the prefix to match is the
			// mount's CONTAINER root without it ("opt/pi"), not its repo directory ("pi") —
			// comparing against inRepo was my first attempt and it silently matched nothing.
			isContainerSpelling := false
			for _, mount := range inContainerMounts {
				root := strings.TrimPrefix(mount.inContainer, "/")
				if rel == root || strings.HasPrefix(rel, root+"/") {
					isContainerSpelling = true
					break
				}
			}
			if isContainerSpelling {
				for _, mount := range inContainerMounts {
					root := strings.TrimPrefix(mount.inContainer, "/")
					if rel != root && !strings.HasPrefix(rel, root+"/") {
						continue
					}
					rest := strings.TrimPrefix(strings.TrimPrefix(rel, root), "/")
					if rest == "" {
						break
					}
					if hasTraversalSegment(rest) {
						t.Errorf("%s: container path `%s` walks out of the mount with `..`", path, rel)
						break
					}
					target := filepath.Join(repoRoot, mount.inRepo, rest)
					if _, err := os.Stat(target); err != nil {
						t.Errorf("%s: references `%s`, which maps to %s and does not exist (%v)",
							path, rel, target, err)
					}
					break
				}
				continue
			}
			// Documentation-relative paths (pi/README.md, AGENTS.md) resolve from the
			// repo root; anything with a directory separator is treated as root-relative
			// because that is how every real reference in these files is written.
			target := filepath.Join(repoRoot, rel)
			if _, err := os.Stat(target); err != nil {
				t.Errorf("%s: references `%s`, which does not exist (%v)", path, rel, err)
			}
		}
	}
}

// TestSkillContainerPathsAreMounted: a path like /opt/data/x is only true while pi/ is
// bind-mounted at /opt/pi. If compose changes, or a file carries a leftover container
// path from the old image, this fails rather than the skill quietly lying.
func TestSkillContainerPathsAreMounted(t *testing.T) {
	for _, path := range skillFiles(t) {
		body := instructionBody(read(t, path))
		for _, m := range containerPathRe.FindAllStringSubmatch(body, -1) {
			// Trailing sentence punctuation is prose, not part of the path: a skill
			// writing "under `/opt/pi`." must not fail on a path named `/opt/pi.`
			p := strings.TrimRight(m[1], ".,:;)\u0022'")
			matched := false
			for _, mount := range inContainerMounts {
				if p != mount.inContainer && !strings.HasPrefix(p, mount.inContainer+"/") {
					continue
				}
				matched = true
				// The file itself must exist in the tree the mount points at. Checking
				// only the mount would pass `/opt/pi/skills/nope/SKILL.md`, which is the
				// failure this rule exists for wearing a valid prefix.
				rest := strings.TrimPrefix(strings.TrimPrefix(p, mount.inContainer), "/")
				if rest == "" {
					break // the mount root itself; nothing further to resolve
				}
				// Resolution only works for a mount whose directory is IN this checkout.
				// `workspace/` is where the story and resumes repos are cloned on a live
				// host; it is gitignored, so CI has no such directory and a skill naming
				// `/workspace/portals` would fail there while passing here. Skipped and
				// logged, never silently: the mount itself is still checked above, and the
				// log line is how a reader tells "verified" from "not checkable here".
				root := filepath.Join(repoRoot, mount.inRepo)
				if _, err := os.Stat(root); err != nil {
					t.Logf("%s: skipping resolution of `%s` — %s is not in this checkout (%v)",
						path, p, mount.inRepo, err)
					break
				}
				// `..` is rejected rather than cleaned. A path that walks out of the mount
				// and back in (`/opt/pi/../pi/skills/x.md`) resolves to something real while
				// claiming a location it does not have, and cleaning it would quietly bless
				// a path whose author meant a different directory.
				// Segment-wise, not `strings.Contains`: a filename may legitimately contain
				// a double dot (`report..final.md`), and flagging that would push an author
				// towards writing a path that lies about its own name.
				if hasTraversalSegment(rest) {
					t.Errorf("%s: container path `%s` walks out of the mount with `..` — "+
						"name the file relative to %s instead", path, p, mount.inContainer)
					break
				}
				target := filepath.Join(root, rest)
				if _, err := os.Stat(target); err != nil {
					t.Errorf("%s: references container path `%s`, which maps to %s and does "+
						"not exist (%v)", path, p, target, err)
				}
				break
			}
			if !matched {
				t.Errorf("%s: references container path `%s`, which is not under any bind "+
					"mount — either the mount moved or the path is wrong", path, p)
			}
		}
	}
}

// TestSkillHostToolingIsLabelled: the exact defect this package exists for. A skill
// whose commands assume the VPS host, loaded by an agent inside a container that has
// no podman, produces "command not found" reported as a broken stack.
func TestSkillHostToolingIsLabelled(t *testing.T) {
	for _, path := range skillFiles(t) {
		// Frontmatter is metadata, not instructions. A `description:` legitimately
		// mentions tool names, and scanning it would flag a file for describing itself
		// rather than for telling the agent to run something.
		sections := sectionsOf(instructionBody(read(t, path)))
		// A caveat in the OPENING section covers the whole file: "read this first, you
		// are not on the host" at the top is a clearer way to say it than repeating the
		// caveat in every section, and an agent reads it before anything else.
		//
		// "Opening", not literally the preamble: a file whose first line is `# Title` puts
		// that caveat in its first *section*, and holding it to a stricter standard would
		// fail a correctly-written file over where a heading happened to fall. What is not
		// allowed is a caveat halfway down next to one command, which is the case a
		// reader reaches after the unlabelled one above it.
		//
		// Frontmatter no longer counts at all — it is metadata, stripped before sectioning.
		// It used to, which is how a `description:` mentioning "docker" was silently
		// discharging this rule for the whole file.
		// Two ways to satisfy the host-tooling rule: the section says so itself, or it sits
		// below the file's opening statement of where the file is written from.
		// The OPENING section's caveat is the file's own statement of which side of the
		// container boundary it is on, so it covers the whole file. Every other caveat is
		// scoped to its own section: a sentence halfway down must not silence every command
		// below it, which is the "one marker anywhere" behaviour per-section scoping exists
		// to prevent. Computed once, before the loop, for that reason.
		fileCovers := len(sections) > 0 &&
			hasSplitMarker(sections[0].title+"\n"+sections[0].body)
		for _, section := range sections {
			ownCaveat := hasSplitMarker(section.title + "\n" + section.body)
			// A document's own title is its NAME, not an instruction: this file is called
			// podman-management, and scanning its H1 flagged it for being itself. The title
			// of every other heading is scanned, because `## Restart with docker-compose`
			// with an empty body is exactly where an agent copies a command from.
			scannable := section.body
			if section.level != 1 {
				scannable = section.title + "\n" + section.body
			}
			tools := toolsMentioned(scannable)
			if len(tools) == 0 {
				continue
			}
			// Scoped to the section, unless a caveat above already covers it, or the
			// section declares itself with an explicit fenced host comment.
			if fileCovers || ownCaveat || hasFenceMarker(section.title+"\n"+section.body) {
				continue
			}
			t.Errorf("%s:%d: section %q mentions %v but does not say the commands are for "+
				"the host (expected a phrase like %q, or a %q comment) — the agent runs in "+
				"a container with none of these binaries",
				path, section.startLine, section.title, tools,
				"not on the host / for the human / host only", hostFenceMarker)
		}
	}
}

// hostFenceMarker is the canonical spelling, quoted in failure messages so a reader knows
// what to write; the matching itself is hasFenceMarker, which normalises.
const hostFenceMarker = "# host only"

// headingRe and fenceRe are package-level because sectionsOf runs per file per test and
// recompiling them each time is pure waste.
var (
	// Up to three leading spaces, per CommonMark: a heading inside a list is still a
	// heading, and `  ## podman-compose` is not less of an instruction for being indented.
	headingRe = regexp.MustCompile(`^ {0,3}(#{1,6})\s+`)
	// Both fence styles: a `~~~` block leaves its `#` comments looking like headings,
	// which is the phantom-section bug this tracking exists to prevent — it was backtick
	// fences only, until a second style turned up.
	fenceRe = regexp.MustCompile("^\\s*(`{3,}|~{3,})")
)

// An unclosed fence is not allowed to silence the rest of the file. If one is open when
// the body ends, the split is redone with fence tracking off — a malformed block then
// costs precision, never coverage, which is the only acceptable direction for this rule.
// sectionsOf splits a skill file at EVERY heading, whatever its level, because a tool
// mention can sit under any of them and a nested heading does not change whose
// instructions they are. Preamble text before the first heading is its own section:
// that is where a "read this first" note lives, and it must not be read as covering the
// sections that follow it.
//
// A heading with NO body is still a section. It used to be dropped, which quietly
// deleted exactly the case worth checking — `## Restart with docker-compose` followed by
// nothing is a section whose entire content is an instruction, and an agent copies from
// the heading.
func sectionsOf(body string) []section {
	sections, unclosed := splitSections(body, true)
	if unclosed {
		// Recovery, and it is deliberately lossy rather than silent: a file with an
		// unclosed fence is re-split WITHOUT fence tracking, so its later headings are
		// checked. The alternative — dropping them — turns one malformed block into a file
		// that checks nothing at all, which is the worst direction for this rule to fail
		// in. What is lost is precision inside the unclosed block: a `#` comment in there
		// reads as a heading again. Coverage first.
		sections, _ = splitSections(body, false)
	}
	return sections
}

// splitSections does the work, and reports whether a fence was still open at EOF.
func splitSections(body string, trackFences bool) (sections []section, unclosed bool) {
	type raw struct {
		title string
		level int
		start int
		body  string
	}
	var raws []raw
	lines := strings.Split(body, "\n")

	current := raw{title: "(preamble)"}
	currentStart := 1
	var buf []string
	flush := func() {
		current.body = strings.Join(buf, "\n")
		if current.title != "(preamble)" || strings.TrimSpace(current.body) != "" {
			raws = append(raws, current)
		}
		buf = nil
	}
	// Fence tracking is not optional: a bash comment starts with `#`, so without it
	// every `# comment` line inside a command block reads as a markdown heading and the
	// "section" it opens is a fragment of shell. That is how a skill ends up with
	// sections named "Reports what it would remove, deletes nothing. Safe to run.".
	// The opening style is remembered, so a `~~~` cannot close a ``` block.
	fenceStyle := ""
	for i, line := range lines {
		if m := fenceRe.FindStringSubmatch(line); trackFences && m != nil {
			style := m[1]
			// A CLOSING fence is the fence characters and nothing else: ```bash inside a
			// block is an opening fence with an info string, and treating it as a close
			// ended the block early — so a documented sample could hide the very commands
			// after it from every check in this file.
			isClose := strings.Trim(line, " \t`~") == ""
			switch {
			case fenceStyle == "":
				fenceStyle = style
			case isClose && style[0] == fenceStyle[0] && len(style) >= len(fenceStyle):
				// CommonMark: a closing fence is the same CHARACTER and at least as long.
				// Comparing whole runs meant a longer ``` closed a ~~~ block and a shorter
				// one could not close the block it opened.
				fenceStyle = ""
			}
			buf = append(buf, line)
			continue
		}
		if fenceStyle == "" && headingRe.MatchString(line) {
			current.start = currentStart
			flush()
			trimmed := strings.TrimLeft(line, " ")
			current = raw{
				title: strings.TrimSpace(strings.TrimLeft(trimmed, "#")),
				level: len(trimmed) - len(strings.TrimLeft(trimmed, "#")),
			}
			currentStart = i + 2
			continue
		}
		buf = append(buf, line)
	}
	current.start = currentStart
	flush()
	out := make([]section, 0, len(raws))
	for _, r := range raws {
		out = append(out, section{title: r.title, level: r.level, startLine: r.start, body: r.body})
	}
	return out, fenceStyle != ""
}

type section struct {
	title string
	// level is the heading's depth: 1 for the document title, 2+ for sections. Only the
	// document title is exempt from tool scanning, because it is the skill's name.
	level int
	// startLine is the FIRST LINE OF THE BODY, not the heading — deliberately. A failure
	// message already prints the section title, so the line number exists purely to let a
	// reader jump straight there; pointing it at the content rather than the heading above
	// it is what makes `sed -n 42p` land on the text that failed. Changing this back is a
	// one-line edit that would make every reported line one off from where the reader
	// wants to look, so the intent is recorded here rather than left to be "corrected".
	startLine int
	body      string
}

func toolsMentioned(body string) []string {
	var found []string
	for _, tool := range hostOnlyTools {
		if tool.pattern.MatchString(body) {
			found = append(found, tool.display)
		}
	}
	return found
}

func hasSplitMarker(body string) bool {
	for _, marker := range hostSplitMarkers {
		if marker.MatchString(body) {
			return true
		}
	}
	return false
}

// instructionBody strips the frontmatter block, leaving only what the agent reads as
// instructions.
func instructionBody(body string) string {
	// The SAME opener test frontmatterOf uses — including the tolerance for `---\r\n`
	// and `--- `. Checking `HasPrefix(body, "---\n")` here while frontmatterOf accepted
	// a CRLF file meant a CRLF skill had its frontmatter passed as instructions, which is
	// the exact false positive the strip exists to prevent — the two functions had to agree
	// on what "has frontmatter" means, and they did not.
	first, rest, found := strings.Cut(body, "\n")
	if !found || strings.TrimRight(first, " \t\r") != "---" {
		return body
	}
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " \t\r") != "---" {
			continue
		}
		// Everything after the closing fence line. Arithmetic, not a search for the block
		// text: `strings.Index(body, block)` is wrong when the same text appears twice
		// and undefined when the block is empty (`---\n---`), either of which would leave
		// a stray `---` at the top of the instructions.
		return strings.Join(lines[i+1:], "\n")
	}
	return body
}

// hasTraversalSegment reports whether a `/`-separated path walks upwards. Compared per
// segment so only a segment that IS ".." counts.
func hasTraversalSegment(p string) bool {
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." {
			return true
		}
	}
	return false
}

// hasFenceMarker matches the explicit in-block opt-in, normalised: `# host only`,
// `#host only`, `#  Host-Only` and `# host-only` are one instruction written four ways,
// and an author who writes the fourth should not conclude their caveat did not count.
// Trailing `\b` for the same reason the split markers have one: without it, a comment
// reading `# host onlyfoo` discharges the rule.
var fenceMarkerRe = regexp.MustCompile(`(?i)#\s*host[\s-]*only\b`)

func hasFenceMarker(body string) bool { return fenceMarkerRe.MatchString(body) }

// TestSkillHasNoBareSkillsPath: pi/skills/ and pi/profiles/<n>/skills/ are the two
// places Pi reads. A reference to a bare `skills/...` is either stale or has been
// wrong since the migration, since nothing discovers the repo-root tree.
func TestSkillHasNoBareSkillsPath(t *testing.T) {
	for _, path := range skillFiles(t) {
		body := read(t, path)
		// One pass over the lines rather than one per regex match: iterating matches and
		// then re-scanning the whole file for each produced N^2 errors for N occurrences
		// of the same problem, which buries the other failures in a wall of duplicates.
		for i, line := range strings.Split(body, "\n") {
			// Submatch, not FindString: the pattern's boundary alternative is part of the
			// match, so the failure would quote `" skills/foo/SKILL.md"` with a leading
			// space — a small thing that makes the message look like it is quoting a
			// filename that does not exist.
			if m := bareSkillsPath.FindStringSubmatch(line); m != nil {
				t.Errorf("%s:%d: references a bare `skills/...` path (%q) — nothing discovers "+
					"the repo-root skills tree; use pi/skills/ or pi/profiles/<name>/skills/",
					path, i+1, m[1])
			}
		}
	}
}

// TestSkillEndsWithNewline: a file without one makes the next diff that touches its
// last line show as two changes, which trains people to skim diffs.
func TestSkillEndsWithNewline(t *testing.T) {
	for _, path := range skillFiles(t) {
		body := read(t, path)
		if strings.TrimSpace(body) == "" {
			t.Errorf("%s: empty — a skill with no content loads as nothing and is listed "+
				"as blank rather than refused", path)
			continue
		}
		if !strings.HasSuffix(body, "\n") {
			t.Errorf("%s: no trailing newline", path)
		}
	}
}

// containerCandidateRoots are the roots a container-only path can plausibly sit under.
// They are deliberately BROADER than the declared mounts: the rule's value is catching
// a path that is NOT a mount (`/opt/data/state.db` from the old Hermes layout,
// `/opt/hermes/…`), so a pattern built only from the mounts can never fail on the case
// it exists for — it would only ever confirm what it already assumed.
var containerCandidateRoots = []string{"/opt", "/workspace"}

// containerPathRegexp matches a candidate container path, capturing the WHOLE path.
//
// Two bugs lived here once, both found by reintroducing the defect and watching it
// pass. The capture group used to wrap only the mount alternation, so
// FindAllStringSubmatch returned the root and the rule spent its life asserting that
// `/opt/pi` is a prefix of `/opt/pi/…` — true by construction, checking nothing. And
// the pattern was built from the mounts themselves, which made the "not a mount" branch
// unreachable, so `/opt/data/state.db` sailed past.
// Built once: same reasoning as headingRe/fenceRe.
var containerPathRe = buildContainerPathRegexp()

func buildContainerPathRegexp() *regexp.Regexp {
	roots := make([]string, 0, len(containerCandidateRoots))
	for _, r := range containerCandidateRoots {
		roots = append(roots, regexp.QuoteMeta(strings.TrimSuffix(r, "/")))
	}
	// `+`, not `*`, after the root slash: a bare mention of `/opt/` in prose is not a
	// path, and with `*` it matched and then failed as "not under any bind mount".
	// A left boundary, or `foo/opt/pi/bar` reads as a container path. Same reason
	// bareSkillsPath has one.
	return regexp.MustCompile("(?:^|[^A-Za-z0-9_])((?:" + strings.Join(roots, "|") + ")/[A-Za-z0-9_./-]+)")
}

// frontmatterOf extracts the block between the opening and closing `---` lines.
// Returns false when the file does not open with one, or when it is never closed —
// an unterminated block means the whole file would be treated as frontmatter.
// The closer must be a line of its own. `strings.Index(rest, "\n---")` also matched
// `\n---foo`, so a file whose frontmatter ended in a rule or a stray `---x` was
// accepted with the rest of itself treated as metadata — and a name/description inside
// that region would satisfy the frontmatter checks from the wrong place.
func frontmatterOf(body string) (string, bool) {
	// The OPENER gets the same tolerance as the closer: a CRLF file or a `--- ` first
	// line is valid frontmatter, and rejecting it here while accepting it below would be
	// an inconsistency rather than a rule.
	first, rest, found := strings.Cut(body, "\n")
	if !found || strings.TrimRight(first, " \t\r") != "---" {
		return "", false
	}
	// The closer is a LINE of dashes, with trailing whitespace and CR tolerated: `--- ` and
	// a CRLF file are both valid YAML frontmatter, and failing them would reject a correct
	// file over formatting the agent never sees.
	lines := strings.Split(rest, "\n")
	for i, line := range lines {
		if strings.TrimRight(line, " \t\r") == "---" {
			return strings.Join(lines[:i], "\n"), true
		}
	}
	// No EOF special case: `strings.Split("a\n---", "\n")` already yields "---" as the
	// final element, so the loop above handles a file that is nothing but frontmatter and
	// has no trailing newline. A LastIndex branch for that case could never run.
	return "", false
}
