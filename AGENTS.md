# AGENTS.md

## Hard rule: this is a sandbox project

- NEVER modify, restart, or touch live Hermes state on this machine:
  - `~/.hermes/` (config.yaml, .env, logs, skills, state)
  - `hermes` CLI, `hermes-gateway` systemd unit, `hermes dashboard`
  - Container stack on this machine (compose services) — compose files in
    this repo are the source of truth, but do NOT run `podman-compose` or
    `./scripts/hermes.sh start|stop|restart|init` against the live daemons
    while working here.
- Work only inside this repo. Preview/validate changes here; the user applies
  them to the live machine themselves.
- If a task requires live Hermes action, STOP and ask the user first.

## What this project is

Runs **three Hermes profiles** (story, resumes, default-god) against
OpenCode Zen directly (no proxy). ONE multiplexed gateway process
for all three profiles (Hermes `gateway.multiplex_profiles`, `s6`-supervised),
a built-in OpenAI-compatible API server (:8642) for the custom Android app,
and remote MongoDB for domain data (money, health, cookbook). **The live stack
is fully containerized** — one compose file (`docker/docker-compose.yml`): health-api + one `gateway` container (all 3 profiles, direct to `https://opencode.ai/zen/v1`, `s6` supervised) +
a one-shot retention job. Development runs the SAME single compose file
against Atlas. No host Hermes install, no native processes.

```
story+resumes+default
   └─► ONE `gateway` container (HERMES_HOME=/hermes-home = profiles/master, s6-supervised)
        └─► OpenCode Zen direct (https://opencode.ai/zen/v1, model muse-spark-1.2-contributor-free)
proxy (:8080, single app URL: /p/* → gateway chat, /api/* → health sync)  •  health-api (:8001)
MongoDB (Atlas)  •  retention (one-shot container)
workspace/portals (lore vault, repo vsreddyh/portals) + workspace/resumes (repo vsreddyh/Resume) — separate git repos
```

## Repo facts

- ONLY the root `.env` exists (git-ignored; `.env.example` tracked). Every env
  var for the whole stack lives there — API keys, Mongo
   URI, app API key. compose maps them into each service; there is
  NO `profiles/*/.env`.
- Docs: `README.md` = quick start; `documentation.md` = deep dive.
- `scripts/hermes.sh` = single entry point (`init|start|stop|restart|status|clean`),
  a thin Podman orchestrator over `docker/docker-compose.yml`. No host installs.
  `init` self-installs the host tools it needs: **podman + compose, curl,
  python3, cron** (only git + sudo must pre-exist). Hermes harness only —
  `init` never installs the opencode CLI. health-api (:8001) binds `0.0.0.0` inside its container.
- `scripts/retention.sh` = wrapper for the one-shot `retention` service
  (`podman-compose run --rm retention` → `tools/retention.py`); cron daily 03:00
   installed by `init`, also runs on every `start`. money wipes transactions >90d;
   health-check prunes `hc_meals`/`hc_days` >30d (never `hc_weight`); cookbook is
   permanent; story/resumes (git repos) are no-ops.
- `tools/mongo.py` = shared pymongo CLI; `tools/retention.py` = data lifecycle.
  `MONGODB_URI`/`MONGODB_DB` in root `.env` (Atlas, all environments).
- Podman: `docker/docker-compose.yml` = the whole stack (health-api
   + gateway + retention) — the ONLY compose file. All environments run the
   same file against the Atlas `MONGODB_URI`. The bot image is built from `test/Dockerfile` +
   `test/entrypoint.sh` (bakes in `s6-overlay`; `hermes-agent` + `mcp` via pip; nodejs + headless chromium for the Playwright MCP); those are the image source, not
   a mirror stack.
   Provider keys + `PASSWORD` are injected via compose `environment:` interpolation
   from the root `.env`; `podman_compose()` always passes `--env-file "$REPO/.env"`
   (compose otherwise looks for `.env` in the compose file's dir and every `${VAR}`
   silently falls back empty/default).
- LLM: direct to OpenCode (`https://opencode.ai/zen/v1`, model `muse-spark-1.2-contributor-free`) — no proxy container. One `OPENCODE_API_KEY` in root `.env` covers both per-request providers (`opencode`|`opencode-go`).
- App API: Hermes built-in OpenAI-compatible server on the gateway
  (`platforms.api_server`, `:8642`, single-password `PASSWORD`); one port, each app
  tab uses its profile path (`/p/story|resumes|default`) and sends per-request
  provider (`opencode`|`opencode-go`) + model from app Settings. Provider keys
  live only in the VPS `.env`, never in git.
- Bot config source is `profiles/master/config.yaml.template` (gateway home,
  not a bot) and `profiles/master/profiles/<bot>/config.yaml.template` (the three
  domain profiles — nested because Hermes multiplexes named profiles under the
  gateway home). Templates use `${HERMES_BASE_URL}` and `${HERMES_CWD}` plus
  `${PASSWORD}` on the gateway home.
  `test/entrypoint.sh` renders each to a git-ignored `config.yaml` at container
  start with container defaults: `https://opencode.ai/zen/v1` + `/workspace/<bot>`.
- Skills: project skills live in `skills/` and are copied into every profile on
  init. Hermes skill content (incl. autogenerated + `nousresearch/`), the
  skill-curator learning state (`.curator_state`/`.usage.json`),
  and each `SOUL.md` are **committed** so bot personality + learned state
  survives moving between VPSes. Only transient session/log/state files are
  git-ignored (runtime `memories/` are not tracked).
- CI: `android-apk.yml` (builds debug+release APKs, `main` branch only) and `mcps-test.yml` (pytest over `mcps/`, `main` only; DB tests skip without `MONGODB_URI`). No linter. Verify shell with `bash -n scripts/*.sh` + render a template to /tmp,
  `podman-compose -f docker/docker-compose.yml config`, then check gateway logs on the live machine.
- Git identity: every commit as `vsreddyh <shouryanreddyh@gmail.com>` (`git -c user.name=vsreddyh -c user.email=shouryanreddyh@gmail.com commit ...`). Never use another name/email.

## Agento app versioning (semver — MAJOR.MINOR.PATCH)

- Source of truth is `android/agento/VERSION` (holds `MAJOR.MINOR.PATCH`, nothing else). Gradle reads it with NO fallback (missing/malformed file fails the build); CI reads the same file for the tag, release name/notes, and artifact names. `versionCode` is derived from semver in Gradle (`MAJOR*1000000+MINOR*1000+PATCH`, segments <1000) — never set it by hand, no env needed.
- Version-only scheme, NO build numbers anywhere: tags are `agento-v<version>` (e.g. `agento-v0.3.0`), APKs are `agento-<version>-<type>.apk`, artifacts and release names match. Same-version rebuilds upsert the existing Release. The in-app updater compares semver only (old `-<build>` tags still parse).
- **PATCH** (`x.y.Z+1`): bug fixes with no behavior contract change — crash fix, sync bug, UI text/layout, proguard/R8 tweak. No new prefs keys, no new permissions, no workflow/tag changes.
- **MINOR** (`x.Y+1.0`): backward-compatible features — new screen/section, new OPTIONAL prefs keys (old backups must still import: `SettingsBackup` skips unknown keys, so additive is safe), new permissions that degrade gracefully, new non-breaking server endpoints.
- **MAJOR** (`X+1.0.0`): anything breaking — prefs key renames/removals, `PREFS_NAME` change, `applicationId` change, signing-key change, `SettingsBackup` export `version` bump, tag/scheme change, or a server API contract the old app can't speak (even if the breaking change lives outside `android/`).
- Rules: any PR that changes the built APK (touches `android/**` or `.github/workflows/android-apk.yml` behavior) bumps `VERSION` EXACTLY ONCE at the highest applicable level and resets lower segments to 0. Pure docs/notes-text tweaks don't bump. Non-app PRs (`profiles/`, `docker/`, `scripts/`, `mcps/`, `tools/`, docs) NEVER touch `VERSION` — unless they force an app MAJOR per above.
- CI split: `main` builds/uploads/releases the release APK only; branches/PRs build/upload the debug APK only. A `Verify version consistency` step fails the job if the built APK filename doesn't match the resolved version, so drift can never publish a mislabeled Release.

## Gotchas

- `init` copies `.env.example` → `.env` (single root file) when none exists, then
  tells you to EDIT it — placeholder tokens/URIs won't work until you do.
- `make setup` in docker/ references `docker/.env.example` which doesn't exist;
  compose reads the ROOT `.env` via `--env-file "$REPO/.env"` (there is no
  `env_file:` directive). `make` targets wrap compose in `docker/`.
- Legacy native install: `start` detects stale `run/bots/*.pid` processes and
  stops them first; if a `hermes-gateway` systemd unit survives, `stop`
  best-effort stops it. `clean` no longer touches `~/.hermes` (no host install).
- Migration (first container start after the native era): stop old native bots /
  health-api before `./scripts/hermes.sh start`, or two
  gateways will fight over the same ports.
- Remote MongoDB is never touched by `clean`. Creds live only in git-ignored `.env`.
- Dev isolation = point `MONGODB_URI`/`MONGODB_DB` at a separate throwaway Atlas database — there is no local MongoDB service.
