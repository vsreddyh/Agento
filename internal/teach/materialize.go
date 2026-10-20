package teach

import (
	"os"
	"path/filepath"
	"sort"
)

// RuntimeDir is where one profile's taught skills materialize:
// <agentDir>/.runtime/skills/<profile>/<name>/SKILL.md. Under .runtime so it
// is never committed (git-ignored like the gateway's runtime state); Mongo
// is the source of truth and this tree is rebuilt on every gateway start.
func RuntimeDir(agentDir, profile string) string {
	return filepath.Join(agentDir, ".runtime", "skills", profile)
}

// Materialize reconciles dir with the profile's ACTIVE skills: it writes one
// SKILL.md per active skill and removes anything else (disabled skills,
// deleted skills, renamed skills). Idempotent: unchanged files are rewritten
// with identical bytes, so Pi restarts are the only reload boundary.
//
// Only the profile's own directory is touched; sibling profiles and the
// committed skills tree are never listed, let alone removed.
func Materialize(dir string, skills []Skill) (int, error) {
	active := make([]Skill, 0, len(skills))
	for _, s := range skills {
		if s.Status == StatusActive {
			active = append(active, s)
		}
	}
	sort.Slice(active, func(a, b int) bool { return active[a].Name < active[b].Name })
	want := make(map[string]bool, len(active))
	written := 0
	for _, s := range active {
		// Belt and suspenders: names are validated on write, but a
		// hand-edited row with "../" must never escape dir through Join.
		if !ValidName(s.Name) {
			continue
		}
		want[s.Name] = true
		skillDir := filepath.Join(dir, s.Name)
		// A file where the skill dir belongs (never created by this
		// reconciler, but disks surprise) would wedge MkdirAll and with it
		// every later skill: remove it and proceed, noisily.
		if st, err := os.Stat(skillDir); err == nil && !st.IsDir() {
			if err := os.Remove(skillDir); err != nil {
				return 0, err
			}
		}
		if err := os.MkdirAll(skillDir, 0o755); err != nil {
			return 0, err
		}
		if err := os.WriteFile(filepath.Join(skillDir, "SKILL.md"), []byte(RenderSKILL(s)), 0o644); err != nil {
			return 0, err
		}
		written++
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return written, nil
		}
		return 0, err
	}
	for _, e := range entries {
		name := e.Name()
		if name == "." || name == ".." || want[name] {
			continue
		}
		// Only directories this reconciler could have created are removed:
		// a stray file (README, .DS_Store) is left alone, exactly like the
		// --skill existence check ignores non-skill entries.
		if !e.IsDir() {
			continue
		}
		if err := os.RemoveAll(filepath.Join(dir, name)); err != nil {
			return 0, err
		}
	}
	return written, nil
}
