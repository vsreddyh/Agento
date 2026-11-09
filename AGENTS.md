# AGENTS.md

## Hard rule: this repo on this machine IS the live stack

- This checkout is the live deployment: the container stack
  (`docker/docker-compose.yml`) runs from here, and the agent side
  (`opencode.json` + `.opencode/`) runs from this checkout too. There is no
  separate "live machine" — changes here affect production.
- `gateway/` holds the *former* Hermes runtime state (transcripts, sqlite WALs,
  caches) and nothing reads it any more, but it is not in git and cannot be
  recovered from git, so it is left on disk. NEVER commit runtime state: only
  edit source files (templates, scripts, compose, code, docs). Check `git status` before staging and leave
  state files (`.pid`, `.heartbeat`, `logs/`, `*.db*`, `cache/`,
  `.curator_state` churn) uncommitted.
- Read-only live inspection is always fine: `podman ps/logs`,
  `scripts/hermes.sh status`, reading logs/config, `curl` against
  localhost ports, rendering templates to /tmp.
- Disruptive actions need explicit user go-ahead first (STOP and ask):
  `start|stop|restart|init|clean`, recreating containers, editing the live
  `.env`, touching MongoDB data. No surprise restarts.
- There is no agent container any more (#494): T3/opencode runs on the host,
  and the compose stack below is `health-api` + `proxy` + `retention` only.
  The script name `scripts/hermes.sh` is a leftover from the Hermes era and is
  kept so the cron/sysmon helpers and the docs keep pointing at something real.

## What this project is

A **thin Android client** — Tasks/Projects, Task Manager, Files, ∃-Mail, Health
Sync, reminders/widgets — plus the small Go backend it talks to: `health-api`
(sync, tasks, projects, files, reports), an nginx proxy, and a one-shot retention
job. **The live stack is fully containerized** — one compose file
(`docker/docker-compose.yml`) — against remote MongoDB Atlas. Development runs
the SAME single compose file against Atlas. There is no agent service in it.

The agent runs **on the host in T3/opencode**, configured by `opencode.json`
and `.opencode/`, with the six kept MCP servers built from this repo
(`./scripts/build-mcps.sh` → `bin/<server>`) reading the same Atlas database.

```
Agento (Android) ──► proxy (:8080 compose default — APP_PORT remaps)
                       └─► /api/* + /health → health-api (:8001 compose default — HEALTH_HOST_PORT remaps)
T3/opencode (host) ──► bin/{task-manager,project-manager,miser-money,cookbook,health-check,reports}
MongoDB (Atlas)  •  retention (one-shot container)
workspace/portals + workspace/portfolio-resume stay on disk, orphaned (retired story/resumes checkouts, unmanaged)
```

## Repo facts

- ONLY the root `.env` exists (git-ignored; `.env.example` tracked). Every env
  var for the whole stack lives there — provider key, Mongo URI, app password.
  compose maps them into each service; there are no per-profile env files.
- Docs: `README.md` = quick start; `documentation.md` = deep dive.
- `scripts/hermes.sh` = single entry point (`init|start|stop|restart|status|clean`),
  a thin Podman orchestrator over `docker/docker-compose.yml`. No host installs.
  `init` self-installs the host tools it needs: **podman + compose, curl,
  python3, cron** (only git + sudo must pre-exist). Container harness only —
  `init` never installs the opencode CLI. health-api (`:8001` compose default —
  `HEALTH_HOST_PORT` remaps) binds `0.0.0.0` inside its container.
- `scripts/retention.sh` = wrapper for the one-shot `retention` service
  (`podman-compose run --rm retention` → `cmd/retention` Go binary); cron daily 03:00
   installed by `init`, also runs on every `start`. money wipes transactions >90d;
   health-check prunes `hc_meals`/`hc_days` >30d (never `hc_weight`); cookbook is
   permanent; reports (∃-Mail) prunes read+unstarred >7d (never unread/starred).
   It also **reconciles** tasks: completed structured repeats with no successor
   are REPORTED (#180), never repaired — rollover is a side effect of `Complete`,
   so a repeat can stop recurring with nothing else noticing.
- `cmd/mongo` = shared MongoDB CLI; `cmd/retention` = data lifecycle (both Go).
  `MONGODB_URI`/`MONGODB_DB` in root `.env` (Atlas, all environments).
- Podman: `docker/docker-compose.yml` = the whole stack (health-api
   + proxy + retention) — the ONLY compose file. All environments run the
   same file against the Atlas `MONGODB_URI`. `PASSWORD` and the Mongo
   variables are injected via compose `environment:` interpolation from the
   root `.env`; `podman_compose()` always passes `--env-file "$REPO/.env"`
   (compose otherwise looks for `.env` in the compose file's dir and every
   `${VAR}` silently falls back empty/default). The Hermes `gateway` service,
   `test/Dockerfile`, `docker/pi/`, `cmd/pi-gateway/` and the tracked
   `gateway/` config are gone; `gateway/` on disk is untracked runtime state only.
- Agent: `opencode.json` wires the six kept MCP servers (`task-manager`,
  `project-manager`, `miser-money`, `cookbook`, `health-check`, `reports`) to
  `./bin/<server>` with the two Mongo env vars; `.opencode/chat.md` is the
  persona loaded through `instructions`, and `.opencode/skills/` holds the
  shared skills. `./scripts/build-mcps.sh` builds the binaries into git-ignored
  `bin/`. The five retired servers (`notes`, `memory`, `browser`, `gmail`,
  `calendar`) are still in the source tree until #495 and are NOT wired.
- LLM: T3/opencode talks directly to OpenCode Go
  (`https://opencode.ai/zen/go/v1`); one `OPENCODE_API_KEY` in the root `.env`.
  No proxy container and no in-repo model config.
- App API: `health-api` (`cmd/health-api`) on `:8001` compose default
  (`HEALTH_HOST_PORT` remaps), single-password `PASSWORD`; the proxy (`:8080`,
  `APP_PORT` remaps) is the app's one URL and routes `/api/*` + `/health`.
  Chat, resumable runs and the pi-gateway surface (`/p/*`, `:8643`) are gone
  with #494. The provider key lives only in the git-ignored `.env`.
- CI: `android-apk.yml` (unit tests + lint on PRs; release APK on `main`) and `mcps-test.yml` (`go build` + `go vet` + `go test ./...` over `cmd/`/`internal/`, `main` only; DB tests skip without `MONGODB_URI`). `./gradlew lintDebug` (release variant on `main`) fails on errors; the report uploads always. Fix what it reports rather than baselining. Verify shell with `bash -n scripts/*.sh` + render a template to /tmp,
  `podman-compose --env-file "$REPO/.env" -f docker/docker-compose.yml config`, then check health-api logs on the live machine.
- PR review loop (`opencode-review.yml`): every push to an open PR triggers an automatic review (usually ~2 min; timeout 10 min; a new push cancels the in-flight one). While waiting for review/checks, sleep between polls (`sleep 60`, then `gh pr checks` / `gh pr view --comments`) — never busy-loop. Verify each finding before fixing, then fix correctness, security, CI, documented rules and docs. Nothing real → ask the user to merge.
- Android is CI-only: NEVER install an Android SDK, `local.properties`, or Gradle caches on this machine (no `./gradlew` builds/tests/lint locally — there is no local SDK and none must be created). App verification happens exclusively through `android-apk.yml` on the pushed PR (unit tests + lint; the release APK assembles on `main` only): push, read the CI logs, fix, push again. `android/agento/local.properties` is git-ignored; never commit it, never recreate it locally.
- Git identity: every commit as `vsreddyh <shouryanreddyh@gmail.com>` (`git -c user.name=vsreddyh -c user.email=shouryanreddyh@gmail.com commit ...`). Never use another name/email.
- Agent model: always run agent sessions on `muse-spark-1.3-contributor` ONLY (cheapest; everything else is too expensive). This is the agent-side rule — the Android app default (`mimo-v2.6-flash`) is separate and stays.
- Git workflow (no exceptions): ALL changes land on `main` via branch + PR — never commit or push directly to `main`, not even one-line fixes. After a PR merges, delete the branch locally AND remotely (`git branch -d <b>` + `git push origin --delete <b>`), then prune (`git fetch --prune`). Wipe any other branches already merged into `main` at the same time.
- **The Nth PR of a piece of work targets the branch of PR N−1, never `main`.** Start PR 1 against `main`; every PR after it targets the previous PR's branch, all the way up. Merge strictly bottom-up, one at a time. This holds for the Nth PR — it is not a "second PR" special case. PR 3 targets PR 2's branch whether or not PR 1 has merged yet. Why: two open PRs that both edit `CHANGELOG.md` and `android/agento/VERSION` will conflict the moment either merges under the other, and out-of-order merges produce a version whose changelog header does not match the version file. GitHub reports this as `DIRTY`, not as a failure, so it looks like a transient state and gets missed. Concretely: two app PRs at `4.14.5` and `4.14.6` both open against `main` means whichever merges second is no longer a valid version bump until rebased, and the rebase conflicts in `CHANGELOG.md` are indistinguishable from real ones. A stacked chain also makes the dependency explicit — `mergeable: true` against another PR's branch means "safe once the one below lands", whereas `true` against `main` means "safe now" and is a claim that is false whenever two PRs touch the same file.

## Agento app versioning (semver — MAJOR.MINOR.PATCH)

- Source of truth is `android/agento/VERSION` (holds `MAJOR.MINOR.PATCH`, nothing else). Gradle reads it with NO fallback (missing/malformed file fails the build); CI reads the same file for the tag, release name/notes, and artifact names. `versionCode` is derived from semver in Gradle (`MAJOR*1000000+MINOR*1000+PATCH`, segments <1000) — never set it by hand, no env needed.
- Version-only scheme, NO build numbers anywhere: tags are `agento-v<version>` (e.g. `agento-v0.3.0`), APKs are `agento-<version>-<type>.apk`, artifacts and release names match. Same-version rebuilds upsert the existing Release. The in-app updater compares semver only (old `-<build>` tags still parse).
- **PATCH** (`x.y.Z+1`): bug fixes with no behavior contract change — crash fix, sync bug, UI text/layout, proguard/R8 tweak. No new prefs keys, no new permissions, no workflow/tag changes.
- **MINOR** (`x.Y+1.0`): backward-compatible features — new screen/section, new OPTIONAL prefs keys (old backups must still import: `SettingsBackup` skips unknown keys, so additive is safe), new permissions that degrade gracefully, new non-breaking server endpoints.
- **MAJOR** (`X+1.0.0`): anything breaking — prefs key renames/removals, `PREFS_NAME` change, `applicationId` change, signing-key change, `SettingsBackup` export `version` bump, tag/scheme change, or a server API contract the old app can't speak (even if the breaking change lives outside `android/`).
- Rules: any PR that changes the built APK (touches `android/**` or `.github/workflows/android-apk.yml` behavior) bumps `VERSION` EXACTLY ONCE at the highest applicable level and resets lower segments to 0. Pure docs/notes-text tweaks don't bump. Non-app PRs (`.opencode/`, `docker/`, `scripts/`, `mcps/`, `cmd/`, `internal/`, docs) NEVER touch `VERSION` — unless they force an app MAJOR per above.
- CI split: `main` builds/uploads/releases the release APK; branches/PRs run unit tests + lint only (no APK artifact, no upload). A `Verify version consistency` step (main-only) fails the job if the built APK filename doesn't match the resolved version, so drift can never publish a mislabeled Release.

## Agent skills

### Issue tracker

Issues live in this repo's GitHub Issues (`vsreddyh/Agento`, via the `gh` CLI). See `docs/agents/issue-tracker.md`.

### Triage labels

Five canonical triage roles, mapped onto the existing `P0`–`P3` / `area:*` / `size:*` vocabulary; `needs-decision` doubles as `needs-info`. See `docs/agents/triage-labels.md`.

### Domain docs

Single-context: `GLOSSARY.md` + `docs/adr/` at the repo root, created lazily. See `docs/agents/domain.md`.

## Gotchas

- `init` copies `.env.example` → `.env` (single root file) when none exists, then
  tells you to EDIT it — placeholder tokens/URIs won't work until you do.
- `make setup` in docker/ references `docker/.env.example` which doesn't exist;
  compose reads the ROOT `.env` via `--env-file "$REPO/.env"` (there is no
  `env_file:` directive). `make` targets wrap compose in `docker/`.
- A leftover `hermes-gateway` systemd unit is best-effort stopped by `stop`;
  it is inert on a host that never had one.
- Build the MCP binaries before launching T3/opencode: `./scripts/build-mcps.sh`
  writes `bin/<server>`, and `opencode.json` runs them from the repo root — a
  different cwd breaks every server command.
- Remote MongoDB is never touched by `clean`. Creds live only in git-ignored `.env`.
- Dev isolation = point `MONGODB_URI`/`MONGODB_DB` at a separate throwaway Atlas database — there is no local MongoDB service.
