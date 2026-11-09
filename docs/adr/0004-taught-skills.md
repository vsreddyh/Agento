# 0004 — User-taught skills

Status: accepted (#314, server half)

- Superseded by: [ADR-0012](0012-t3-opencode-migration.md) (#496, 2026-11) as it relates to the removed Pi surfaces; kept on disk as history, not rewritten.

## Context

Agent skills ship in git (`pi/skills/*`, `pi/profiles/<name>/skills/*`) and
bake into the image. The user cannot teach the agent anything: a corrected
procedure evaporates with the thread, and the Skills tab is a read-only
catalog. The expected loop (#314) is: save a procedure from the app → review
and edit it → test-run it once → activate it for the model. (Promoting an
active skill to a scheduled routine was removed in #490; the app-side
Schedule UI removal is planned in #491 / PR #506.)

## Decision

**Mongo is the source of truth; the filesystem is a cache; git never sees
taught skills.**

- `internal/teach` owns the model (`Skill`, `Status`, name rules, SKILL.md
  rendering) and two backends: Mongo (`taught_skills`) and an in-process map.
  A skill is per-profile, named `[a-z0-9-]`, and carries `draft → active →
  disabled` status plus the last test transcript.
- The gateway materializes ACTIVE skills to
  `<agentDir>/.runtime/skills/<profile>/<name>/SKILL.md` (git-ignored,
  rebuilt on every gateway start and after every mutation) and passes that
  root as a second `--skill` argument to Pi. Committed skills and taught
  skills meet only in the `v1/skills` list (taught rows marked
  `scope:"taught"`) and on the model via separate `--skill` roots.
- Drafts are inert by construction: activation requires a recorded test
  (`testedAt`), enforced in the store, not the UI. Disabling removes the
  file on the next reconcile; deleting removes the row.
- A test fire runs the rendered procedure once through a bare model client
  (no persona, no tools — the client the retired scheduler used for fires
  before #331, kept for test fires after the scheduler's deletion in #490)
  and stores the transcript. It is a smoke test,
  honestly scoped: the runner has no MCP tools, so a pass means the
  procedure reads coherently, not that it ran against real data.
- Promoting created a cron job whose prompt embeds the procedure (removed
  in #490: `POST /api/skills/{id}/promote` is gone, so nothing can promote).
  The paragraph below describes the retired behavior, kept for the record:
  the job's own pause and 10-run history then governed it; pausing a routine
  never touched the skill, and disabling a skill never touched promoted jobs
  (they carry a copy of the reviewed text, so a later edit cannot silently
  change what runs at 9am).

### Why not write into `pi/profiles/<name>/skills/`

That tree is committed. Runtime writes there would dirty the checkout on the
live machine (which deploys from git), and a rebuild would either wipe them
or — worse — tempt someone into committing them. The `.runtime` cache has
the opposite failure mode: losing it loses nothing, because the next start
rebuilds it from Mongo.

### Availability lag, stated plainly

Pi reads `--skill` roots at startup. Activating a skill materializes its
file immediately and lists it in `v1/skills` immediately, but the model
loads it on the next Pi start. The Skills tab is the review surface, not a
promise that this turn already knows the procedure.

### Fallback honesty

Without Mongo the routes serve from an in-process store, but activation
cannot survive a restart and the files are the only copy Pi ever sees — so
an activation would silently un-teach on the next boot. Startup logs that
plainly. Skill tests additionally need `OPENCODE_API_KEY`; without it the
test route answers 502 rather than faking a run.

## Non-goals

- Deduplication of taught procedures (same rationale as memory: pending
  drafts are visible and reviewed one by one).
- Versioning taught skills; the edit history is the transcript + timestamps.
- Multi-profile skill sharing; a skill belongs to exactly one profile.
