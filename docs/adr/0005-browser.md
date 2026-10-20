# 0005 — Browser tasks

Status: accepted (#312, server half)

## Context

The agent cannot touch anything without an MCP, and Pi — the agent host —
ships no browser: its built-ins are read, bash, edit and write. Saying
"the agent has browser tools" was true of the operator's harness, never of
the deployed agent, and no user-visible job has ever finished a real web
task. #312 v1 closes that gap server-side: chat-driven browsing
("open this site, find X, save the evidence") with cited URLs and files in
`exports/`, which the Storage tab already browses. No app changes: the turn
carries the paths, the tab shows the files.

## Decision

**A headless-Chromium MCP server with four tools, gated navigation, and no
form interaction.**

- `cmd/browser` drives system Chromium through chromedp (headless,
  `--no-sandbox`: no display exists and the container runs as root, which
  the sandbox refuses). One persistent browser context per agent profile
  under `.runtime/browser/<profile>/`, so cookies and technical state
  survive across turns; idle profiles prune after 30 days at server start.
- `browser_goto(url)` holds for approval with the URL in front of the
  human. That human decision **is** the domain allowlist: no static list to
  rot, and re-navigation to a new URL holds again. `browser_read`,
  `browser_screenshot` and `browser_save_evidence` run free once the page
  is approved.
- `browser_save_evidence(note?)` writes a timestamped bundle —
  `shot.png` (full page), `page.md` (text + source URL), `meta.json`
  (profile, URL, title, time, note) — jailed under `exports/browser/` with
  generated names. The answer cites those paths and URLs.
- There are deliberately no click/type tools in v1. A login wall therefore
  stops the task with a report instead of accepting pasted passwords, and
  "the user takes over for 2FA" stays a documented non-goal until a headed
  or CDP-attached mode exists. Unattended purchasing is out of scope with
  or without forms.

### SSRF is fenced per connection, not per URL

The container shares a network with cloud metadata and local services, and
a URL can read as harmless text while resolving somewhere it must not go.
`checkURL` allows only http(s) without credentials and refuses every
non-public resolution — but that only covers the first URL. Redirects,
meta-refreshes and DNS rebindings would walk around a check-then-use gate,
so every byte Chromium sends goes through an in-process CONNECT/forward
proxy that re-resolves and re-checks per connection: loopback, RFC 1918,
CGNAT, TEST-NET, link-local (169.254.169.254), multicast, unspecified and
reserved ranges, each pinned by test rather than inherited from the Go
release's tables. HTTPS travels as opaque tunnels (hostnames only, never
plaintext). The approval sheet never sees refused targets — they fail in
the server with the reason attached.

### Accepted blast radius

Headless Chromium runs as container root with `--no-sandbox` (the sandbox
refuses uid 0) and JavaScript enabled on attacker-controlled pages, with
writable mounts on `../pi` and `../exports` and read-only SSH keys. A
renderer exploit reaches all of that. Accepted because the alternative —
no browser capability — is the feature not existing; not accepted
blindly: navigation itself is human-approved per domain, non-public
targets never dial, evidence writes are jailed to `browser/` with generated
names, and disabling JS or adding a seccomp profile stays open as
hardening. Evidence has no quota cap (full-page PNGs accumulate): disk
pressure on `../exports` is a follow-up, not this PR.

### The navigation log is the audit trail

Every MCP call is already audited with its arguments, so every `goto` lands
in `audit_log` with its URL for 90 days. No separate nav log exists to
diverge from it; retention's pruning covers the same collections.

### Caps, stated

- Each step gets its own 30s budget rather than sharing one deadline:
  a `goto` (navigate + title + location) runs up to 90s, `read` up to 90s,
  evidence up to 120s; the turn's own timeout bounds the task. First launch
  serializes concurrent tools per profile behind a single-flight gate —
  one slow Chromium start holds the others, accepted for v1 because
  launches are seconds and rare.
- Extracted text: 12k runes per read (response says when it clipped).
- Session TTL: 30 days idle. Pruned at server start (stray dirs) and
  evicted daily-lazily in use (live Chromium + listener + dir go together);
  a restart never finds anything the evictor would have taken.
- Evidence: PNG + markdown + meta per save; no size cap beyond the page
  itself, and full-page screenshots of long pages are large by nature.
  Working `shot-*.png` files accumulate in the session dir alongside
  evidence (same 30d TTL reaps both); only evidence bundles reach exports.
  Only `goto` is approval-gated, so free `save_evidence` + no quota means
  disk fill is one approved `goto` away (agent loop saving full-page PNGs);
  the 30d TTL reaps it, and a size quota is the follow-up.

### Fallback honesty

Without Mongo nothing here changes: the browser server keeps no database
state (sessions are files, evidence is files), and approvals still gate
through the shared store or fail fast without it. Chromium missing fails
the first tool call loudly rather than parking an approval for a browser
that can never start.

## Non-goals

- Click/type/form interaction (and therefore attended logins, 2FA
  takeover, purchasing).
- A static domain allowlist (per-task human approval is the allowlist).
- Semantic readability extraction (innerText + caps, not a reader mode).
- An app attach button or in-bubble thumbnails (a later app PR on top of
  these same server verbs).
