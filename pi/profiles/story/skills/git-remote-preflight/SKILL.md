---
name: git-remote-preflight
description: "Use when a session must push to a git remote."
---

# Git Remote Preflight

Run before edit work ending in commit + push. Failures found early cost one
command; found after edits, they strand the work.

1. `git -C /workspace/portals status --short --branch` (branch, dirt)
2. `git -C /workspace/portals ls-remote origin` (read-only proof of auth)
3. Clone-or-update: `pull --ff-only`, clone only when `.git` is absent.

One simple command per call — compound bodies trip the command scanner.

- **Host key verification failed**: agent `known_hosts` is read-only; cannot
  fix from the container. Report and stop. Never suggest
  `StrictHostKeyChecking=no`.
- **Permission denied (publickey)**: diagnose once (`ls -la /root/.ssh`,
  `git config user.name/email`), report, stop. Never print `git remote -v`
  (may leak a token URL).
