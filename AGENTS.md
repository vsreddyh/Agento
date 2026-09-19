# AGENTS.md

## Hard rule: this is a sandbox project

- NEVER modify, restart, or touch live Hermes state on this machine:
  - `~/.hermes/` (config.yaml, .env, logs, skills, state)
  - `hermes` CLI, `hermes-gateway` systemd unit, `hermes dashboard`
  - Docker stack on this machine (`searxng`) — compose files in
    this repo are the source of truth, but do NOT run `docker compose` or
    `./scripts/hermes.sh start|stop|restart|init` against the live daemons
    while working here.
- Work only inside this repo. Preview/validate changes here; the user applies
  them to the live machine themselves.
- If a task requires live Hermes action, STOP and ask the user first.

## What this project is

Runs **three Hermes profiles** (story, resumes, default-god) against
OpenCode Zen directly (no proxy). Private SearXNG, ONE multiplexed gateway process
for all three profiles (Hermes `gateway.multiplex_profiles`), a password-protected dashboard
(supervised alongside gateway via `s6`, `HERMES_DASHBOARD=1` in the same `gateway`
container — mirrors official `nousresearch/hermes-agent`),
a built-in OpenAI-compatible API server (:8642) for the custom Android app,
and remote MongoDB for domain data (money, health, cookbook). **The live stack
is fully Dockerized** — one compose file (`docker/docker-compose.yml`): searxng
+ health-api + one `gateway` container (all 3 profiles + dashboard, direct to `https://opencode.ai/zen/v1`, `s6` supervised) +
a one-shot retention job (plus ephemeral `mongodb` in dev). Development runs the SAME single compose file;
`HERMES_ENV=dev` in the root `.env` switches every data consumer to a temporary
local `mongodb` container (`mongodb://mongodb:27017`, no volume, ephemeral).
No host Hermes install, no native processes.

```
story+resumes+default
   └─► ONE docker `gateway` container (HERMES_HOME=/hermes-home = profiles/master, HERMES_DASHBOARD=1 via s6)
        └─► OpenCode Zen direct (https://opencode.ai/zen/v1, model muse-spark-1.2-contributor-free)
searxng (:8888)  •  health-api (:8001)  •  dashboard (:9119, password, s6 alongside gateway)
MongoDB (remote prod; ephemeral local in dev)  •  retention (one-shot container)
workspace/portals (lore vault, repo vsreddyh/portals) + workspace/resumes (repo vsreddyh/Resume) — separate git repos
```

## Repo facts

- ONLY the root `.env` exists (git-ignored; `.env.example` tracked). Every env
  var for the whole stack lives there — API keys, Mongo
   URI, dashboard auth, app API key. compose maps them into each service; there is
  NO `profiles/*/.env`.
- Docs: `README.md` = quick start; `documentation.md` = deep dive.
- `scripts/hermes.sh` = single entry point (`init|start|stop|restart|status|clean`),
  a thin Docker orchestrator over `docker/docker-compose.yml`. No host installs.
  `init` self-installs the host tools it needs: **docker + compose, curl,
  python3, cron** (only git + sudo must pre-exist). Hermes harness only —
  `init` never installs the opencode CLI. Dashboard (:9119) and health-api (:8001) bind `0.0.0.0` inside Docker.
- `scripts/retention.sh` = wrapper for the one-shot `retention` service
  (`docker compose run --rm retention` → `tools/retention.py`); cron daily 03:00
   installed by `init`, also runs on every `start`. money wipes transactions >90d;
   health-check prunes `hc_meals`/`hc_days` >30d (never `hc_weight`); cookbook is
   permanent; story/resumes (git repos) are no-ops.
- `tools/mongo.py` = shared pymongo CLI; `tools/retention.py` = data lifecycle.
  `MONGODB_URI`/`MONGODB_DB` in root `.env` for prod; `HERMES_ENV=dev` switches
  to ephemeral local `mongodb` container (`mongodb://mongodb:27017`, no volume)
  via `scripts/lib/common.sh` (`COMPOSE_PROFILES=dev`). `clean` removes the
  ephemeral dev data on `down -v` (or container remove) since there is no volume.
- Docker: `docker/docker-compose.yml` = the whole stack (searxng
   + health-api + gateway (+ dashboard via s6 when HERMES_DASHBOARD=1) + retention) — the ONLY compose file. Dev and
   prod run the same file; `HERMES_ENV=dev` repoints every data consumer at the
   ephemeral local container under the same `MONGODB_DB` name, never the prod DB. The bot image is built from `test/Dockerfile` +
   `test/entrypoint.sh` (bakes in `s6-overlay`; `hermes-agent` + `mcp` via pip); those are the image source, not
   a mirror stack.
   Provider keys + `API_SERVER_KEY` are injected via compose `environment:` interpolation
   from the root `.env`; `docker_compose()` always passes `--env-file "$REPO/.env"`
   (compose otherwise looks for `.env` in the compose file's dir and every `${VAR}`
   silently falls back empty/default).
- LLM: direct to OpenCode Zen (`https://opencode.ai/zen/v1`, model `muse-spark-1.2-contributor-free`) — no proxy container. OpenCode Go is enabled too (`OPENCODE_GO_API_KEY` in root `.env`, selectable per request as provider `opencode-go`).
- App API: Hermes built-in OpenAI-compatible server on the gateway
  (`gateway.api_server`, `:8642`, shared `API_SERVER_KEY`); one port, each app
  tab uses its profile path (`/p/story|resumes|default`) and sends per-request
  provider (`opencode`|`opencode-go`) + model from app Settings. Provider keys
  live only in the VPS `.env`, never in git.
- Bot config source is `profiles/master/config.yaml.template` (gateway home,
  not a bot) and `profiles/master/profiles/<bot>/config.yaml.template` (the three
  domain profiles — nested because Hermes multiplexes named profiles under the
  gateway home). Templates use `${HERMES_BASE_URL}` and `${HERMES_CWD}` plus
  `${API_SERVER_KEY}` on the gateway home.
  `test/entrypoint.sh` renders each to a git-ignored `config.yaml` at container
  start with docker defaults: `https://opencode.ai/zen/v1` + `/workspace/<bot>`.
- Dashboard binds `0.0.0.0:9119` with `HERMES_HOME=/hermes-home` (mounts
   `profiles/master`, supervised alongside gateway via `s6` when `HERMES_DASHBOARD=1`); auth via `HERMES_DASHBOARD_BASIC_AUTH_USERNAME`/`_PASSWORD`/
   `_SECRET` in the root `.env` (a public bind requires an auth provider).
- Skills: project skills live in `skills/` and are copied into every profile on
  init. Hermes skill content (incl. autogenerated + `nousresearch/`), the
  skill-curator learning state (`.curator_state`/`.usage.json`),
  and each `SOUL.md` are **committed** so bot personality + learned state
  survives moving between VPSes. Only transient session/log/state files are
  git-ignored (runtime `memories/` are not tracked).
- CI: `android-apk.yml` (builds debug+release APKs, `main` branch only) and `mcps-test.yml` (pytest over `mcps/`, `main` only; DB tests skip without `MONGODB_URI`). No linter. Verify shell with `bash -n scripts/*.sh` + render a template to /tmp,
  `docker compose -f docker/docker-compose.yml config`, then check gateway logs on the live machine.
- Git identity: every commit as `vsreddyh <shouryanreddyh@gmail.com>` (`git -c user.name=vsreddyh -c user.email=shouryanreddyh@gmail.com commit ...`). Never use another name/email.

## Agento app versioning (semver — MAJOR.MINOR.PATCH)

- Source of truth is `versionName` in `android/agento/app/build.gradle.kts`. `versionCode` is ALWAYS the CI run number — never set it by hand.
- CI tags releases `agento-v<versionName>-<run_number>`; the in-app updater compares semver first, then build number. A higher `versionName` with a lower build still counts as newer.
- **PATCH** (`x.y.Z+1`): bug fixes with no behavior contract change — crash fix, sync bug, UI text/layout, proguard/R8 tweak. No new prefs keys, no new permissions, no workflow/tag changes.
- **MINOR** (`x.Y+1.0`): backward-compatible features — new screen/section, new OPTIONAL prefs keys (old backups must still import: `SettingsBackup` skips unknown keys, so additive is safe), new permissions that degrade gracefully, new non-breaking server endpoints.
- **MAJOR** (`X+1.0.0`): anything breaking — prefs key renames/removals, `PREFS_NAME` change, `applicationId` change, signing-key change, `SettingsBackup` export `version` bump, tag/scheme change, or a server API contract the old app can't speak (even if the breaking change lives outside `android/`).
- Rules: any PR that changes the built APK (touches `android/**` or `.github/workflows/android-apk.yml` behavior) bumps `versionName` EXACTLY ONCE at the highest applicable level and resets lower segments to 0. Pure docs/notes-text tweaks don't bump. Non-app PRs (`profiles/`, `docker/`, `scripts/`, `mcps/`, `tools/`, docs) NEVER touch `versionName` — unless they force an app MAJOR per above.

## Gotchas

- `init` copies `.env.example` → `.env` (single root file) when none exists, then
  tells you to EDIT it — placeholder tokens/URIs won't work until you do.
- `make setup` in docker/ references `docker/.env.example` which doesn't exist;
  compose reads the ROOT `.env` via `--env-file "$REPO/.env"` (there is no
  `env_file:` directive). `make` is only relevant for the searxng service now.
- Legacy native install: `start` detects stale `run/bots/*.pid` processes and
  stops them first; if a `hermes-gateway` systemd unit survives, `stop`
  best-effort stops it. `clean` no longer touches `~/.hermes` (no host install).
- Migration (first Docker start after the native era): stop old native bots /
  dashboard / health-api before `./scripts/hermes.sh start`, or two
  gateways will fight over the same ports.
- Remote MongoDB is never touched by `clean`. Creds live only in git-ignored `.env`.
- Dev isolation = `HERMES_ENV=dev` in the root `.env` uses a temporary local
  `mongodb` container (`mongodb://mongodb:27017`, no volume, ephemeral) for
  bots, health-api, and retention. Prod (or unset) = remote `MONGODB_URI` as-is.
  Keep `HERMES_ENV=dev` on dev machines — dropping it silently points dev at the
  prod DB. Dev machines run the same single compose file with `COMPOSE_PROFILES=dev`.
