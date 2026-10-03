# `pi/` — the Pi agent dir

Mounted at `/opt/pi` as `PI_CODING_AGENT_DIR`. This is Pi's own state
directory, not application code. Everything here is committed except runtime
state (see `.gitignore`).

## Layout

| path | committed | purpose |
|---|---|---|
| `settings.json` | yes | Pi's own knobs — tool selection, retry, cache warming. **Not** personality. |
| `mcp.json` | yes | the five in-repo Go MCP servers: miser-money, cookbook, health-check, task-manager, project-manager. |
| `AGENTS.md` | yes | rules shared by **every** profile. Pi layers this into all of them, so every line is in every request — keep it short and keep only what is true of all three. |
| `profiles/<name>/AGENTS.md` | yes | one per profile (god / story / resumes). Each profile's own instructions and personality. |
| `profiles/<name>/skills/` | yes *(later PR)* | Intended per-profile skills. **Does not load as laid out** — see the profile-skills finding below; needs an explicit `--skill` path per profile. |
| `auth.json` | **no** | provider credentials. `OPENCODE_API_KEY` is injected by compose. |
| `models-store.json` | **no** | model catalog cache Pi refreshes at startup. |
| `sessions/` | **no** | session transcripts. |

### How instructions are layered

Pi loads context files from the agent directory, the working directory, **and every
parent directory of the working directory**. Each profile runs with its cwd set to
`/opt/pi/profiles/<name>/`, so in practice:

- `/opt/pi/AGENTS.md` — the shared layer, loaded for all three profiles
- `/opt/pi/profiles/<name>/AGENTS.md` — that profile's layer
- `/opt/pi/profiles/AGENTS.md` and `/workspace/AGENTS.md` — **would also load**, for
  every profile, if either existed. Neither is committed. This is worth knowing
  before dropping an `AGENTS.md` anywhere above a profile directory: it silently
  becomes part of every profile's system prompt.

Context-file discovery does not require project trust, so an `AGENTS.md` is loaded
even for an untrusted directory. Treat the content of one as untrusted input.

### Wiring constraint for whoever adds the compose service

`docker/pi/entrypoint.sh` runs `pi mcp list` at boot when `mcp.json` exists, and
**calls `die` if any server fails to connect**. That check only became active when
this PR added `mcp.json`, so the pi service must carry:

- `MONGODB_URI` and `MONGODB_DB` — every one of the five servers opens the shared
  Atlas database on startup and fails to connect without them
- `OPENCODE_API_KEY` — already enforced by the entrypoint

`docker/docker-compose.yml` defines two anchors, and merging the wrong one is worse
than not merging at all:

| anchor | contents |
|---|---|
| `&bot-base-environment` | **only** the environment map: Mongo, the OpenCode keys, `HERMES_HOME` |
| `&bot-base` | the whole service base — that environment **plus** `build` (pointing at `test/Dockerfile`), `user`, and `restart` |

The existing `gateway` service merges `*bot-base` wholesale and then re-merges the
environment map under its own key to add `PASSWORD` and `GIT_SSH_COMMAND`. That is
correct for it, because it *is* the Hermes service.

**A pi service must not merge `*bot-base`.** It would inherit
`build.dockerfile: test/Dockerfile` and quietly build the Hermes image instead of
`docker/pi/Dockerfile`. Give it its own `build` and `user`, and merge only the
environment anchor:

```yaml
  pi:
    build:
      context: ..
      dockerfile: docker/pi/Dockerfile
    user: "0:0"
    environment:
      <<: *bot-base-environment   # Mongo + OpenCode keys
      PASSWORD: ${PASSWORD:-}     # the gateway's bearer token
      GIT_SSH_COMMAND: "..."      # needed for the story/resumes clones
```

Omitting the Mongo variables produces a container that exits at boot rather than one
that serves degraded — the right failure, but an abrupt one.

### The `pi` compose service

`docker-compose.yml` has a `pi` service that runs `pi-gateway` from this image. Two
things about it are deliberate, and both were got wrong while writing it:

- **It runs `pi-gateway`, not the image's default CMD.** The image defaults to `pi
  --mode rpc` on stdio, which serves no port. `pi-gateway` is what serves HTTP *and*
  spawns one `pi --mode rpc` child per profile with `profiles/<name>` as its cwd —
  which is the mechanism that makes each agent load its own `AGENTS.md`. So it is
  built into the image and is the container's main process.
- **It listens on 8643, not 8642.** The `gateway` service still owns 8642 until the
  Hermes removal, so both coexist and an app tab can be pointed at either to compare.
  nginx still proxies `/p/*` to `:8642`; moving it is one line and belongs to the
  removal PR.
- **The host port is `PI_HOST_PORT`, not `PI_SERVER_PORT`.** The container always
  listens on 8643; only the published host port moves. The name is deliberate:
  `PI_SERVER_PORT` is what pi-gateway itself reads as its bind port, so sharing it
  invites the obvious wrong fix — someone debugging a port problem sets
  `PI_SERVER_PORT`, which moves only the host side, and if it were then also passed
  into the container the listener would leave the mapped port and the healthcheck
  with it. `PI_HOST_PORT` cannot collide with anything the binary reads.
- **`PI_GATEWAY_ADDR` must not be set on that service.** It wins outright over
  `PI_SERVER_PORT`, so setting it would move the listener off the mapped port and out
  from under the healthcheck, which probes a fixed in-container `:8643`.

Verified end to end in the real container: the entrypoint registers three repos as
`safe.directory`, `pi mcp list` connects all five MCP servers (11/11/9/7/5), all
three profiles are discovered, `/healthz` answers 200, auth rejects both a missing
and a wrong bearer token with 401, and each profile answers from its own
`AGENTS.md`:

```
god     -> 65                  (its weight goal)
story   -> /workspace/portals  (its vault path)
resumes -> tectonic            (its compile tool)
```

### `tectonic`'s cache is not persisted

Tectonic downloads its TeX package bundle on first use — **42 MB** into
`/root/.cache`, which is not a mounted volume. So the first `.tex` compile after
every `podman up` re-downloads it and needs network; later compiles are fast.

Left unfixed deliberately. A persistent cache means either a named volume — which
survives a `TECTONIC_VERSION` bump and can then serve a bundle the new binary cannot
read — or pointing `XDG_CACHE_HOME` into the bind-mounted `pi/` tree, which puts
42 MB of downloaded assets in the working directory. Neither is right by default,
and a slow first compile is the better failure. Add a cache volume if the latency
bothers you, and clear it when bumping the version.

### `tectonic` is installed — this was the one blocking gap

The `resumes` profile compiles every tailored `.tex` to `exports/` with `tectonic`.
`docker/pi/Dockerfile` installs only `git`, `ca-certificates` and `findutils`, so
after the Hermes image goes away **every resume compile fails**. Today it works only
because `tectonic` happens to be in the `hermes-agent` base image that
`test/Dockerfile` builds on — an accident of the base, not a declared dependency.

Verified while writing this: there is **no `tectonic` package in Debian stable**
(checked the Debian package index by name across all suites), so
`apt-get install tectonic` cannot be the fix. The only route is the upstream release
asset:

| | |
|---|---|
| latest release | `tectonic@0.17.0` |
| asset to use | the `-unknown-linux-musl` build, per arch: `x86_64` on amd64, `aarch64` on arm64 (~9.9 MiB each) |
| why musl, not gnu | statically linked, so no new runtime dependencies in a slim Debian image; the `-gnu` asset is ~22 MiB and pulls glibc expectations |
| pin | `TECTONIC_SHA256_AMD64` / `TECTONIC_SHA256_ARM64` |

The arch is mapped from `TARGETARCH` rather than hardcoded, and anything other than
amd64/arm64 is refused with the arch named. An earlier version pinned only the x86_64
triple, so a build on Apple Silicon or an ARM VPS would fetch the Intel asset and
fail at the version probe — loudly, but only after a wasted 10 MB download.

`ARG TARGETARCH` has to be **declared** for podman's automatic value to be visible in
the build step, and the symptom of omitting it is the quiet one: the default falls
back to amd64, an unsupported-arch build succeeds with the Intel asset, and the guard
never fires. Both halves verified by building rather than by reading:

- `--build-arg TARGETARCH=riscv64` → refused, `TARGETARCH='riscv64'. Supported: amd64, arm64.`
- `--build-arg TARGETARCH=arm64` → selects `aarch64-unknown-linux-musl`, digest
  matches the real asset (`sha256sum: OK`), extracts cleanly, then fails with
  `Exec format error` because an aarch64 binary cannot run on this amd64 host. So the
  mapping and the digest are confirmed, but **executing on real arm64 hardware is
  untested** — build with `--platform linux/arm64` on such a host to confirm it.

This was tracked here as a Dockerfile follow-up rather than done in the
profile-instructions PR, so the image change kept its own review. It is now installed,
pinned by version **and sha256** the way `PI_VERSION` and the base-image digests
already are — an unpinned download from the network into a live image is exactly the
supply-chain surface this repo avoids elsewhere. Bump with:

```
podman build --build-arg TECTONIC_VERSION=0.18.0 \
             --build-arg TECTONIC_SHA256_AMD64=<sha256 of the musl asset> \
             --build-arg TECTONIC_SHA256_ARM64=<sha256 of the aarch64 asset> \
             -f docker/pi/Dockerfile .
```

Confirmed compiling in the built image, not merely present: a minimal article
produced a 3.6 KiB PDF at `/probe/out/probe.pdf`.

The `resumes` instruction was deliberately **not** softened to match the gap while it
was open. Telling
the agent to skip compiling because the binary is missing would produce resumes with
no PDF and no error, which is worse than a compile that fails loudly.

Swept all four instruction files for external binaries the profiles tell the agent to
run: only `git` and `tectonic` appear, and both are now in the image. `curl` was added
at the same time as tectonic, not for the profiles but for the compose healthcheck,
which needs an HTTP client in the container and previously had none.

### Tool exposure

The MCP servers are left at `codemode` exposure, which is Pi's default. Their tools
are therefore **not declared to the model**; it reaches them from a `codemode`
script as `tools.mcp__<server>__<tool>({...})`, finding them with `searchTools()`,
`describeTool()` or `describeNamespace()`. That keeps 43 tool definitions out of
every request, which is most of the token saving available here.

The cost is that the model has to write JavaScript to call a tool, and the model
instructions must say so — a profile file that told the agent to call `log_meal`
directly would describe a tool that does not exist in that form. `god`'s
`AGENTS.md` documents the real call shape instead.

## Why no HTTP server here

Pi is a stdio process: `--mode rpc` speaks JSONL on stdin/stdout. The
OpenAI-compatible surface is a separate gateway binary that speaks RPC to Pi
over the container's stdio pair. It is what gets published as a port and routed
through nginx — not `pi` itself.

### What the gateway serves today

| route | served by the Pi gateway | source |
|---|---|---|
| `POST /{profile}/v1/chat/completions` | yes — SSE streaming, and a standard non-streamed completion when `stream` is false | Pi RPC |
| `GET /healthz` | yes — unauthenticated liveness | — |
| `GET /api/model/options` | yes | `get_available_models`, grouped by provider |
| `GET /{profile}/api/model/options` | yes — same handler, per-profile | `get_available_models` |
| `GET /{profile}/v1/skills` | yes | `get_commands`, filtered to `source == "skill"` |
| `GET /{profile}/v1/toolsets` | yes | `<agent-dir>/mcp.json` |

All three inventory routes were capability loss until this PR: `ChatApi.fetchCatalog`
and both `ServerApi` calls request them, and nginx routes them to the gateway
upstream (`docker/proxy/nginx.conf` sends `/p/*` and both `/api/model/options`
spellings there). They are read-only, bearer-authenticated like chat, and answer 405
with `Allow: GET` on the wrong verb.

### Why those sources and not a filesystem scan

- **Models** come from `get_available_models`, so the picker lists what the agent can
  actually switch to. Verified live: 2 providers, 79 + 29 models.
- **Skills** come from `get_commands` filtered to `source == "skill"`, which reports
  what Pi *actually loaded*, with the path and scope it came from. A filesystem scan
  would be easier and wrong: Pi discovers skills from the agent directory and from
  project directories that need trust, so a directory of `SKILL.md` files is not the
  same thing as a loaded skill. This is not hypothetical — see below.
- **Toolsets** come from `mcp.json`, which is the configured set the entrypoint has
  already proven connectable (it runs `pi mcp list` at boot and refuses to start on
  failure). Pi exposes no RPC that enumerates tools or MCP servers, so per-server
  tool lists are omitted rather than sent empty: the app reads a missing array as
  "unknown" and an empty one as "this server has no tools", and only the first is
  true.

`/v1/skills` currently returns `[]`, and that is the correct answer rather than a
stub: no skills are installed yet. See the profile-skills finding below.

### Profile skills do NOT load from `profiles/<name>/skills/`

This README claimed they did, on the grounds that each session's cwd is the profile
directory. **Measured, that is false.** With one distinctly-named skill planted per
candidate location and `PI_CODING_AGENT_DIR` set explicitly:

| location | discovered |
|---|---|
| `<agent-dir>/skills/` | **yes** |
| `<cwd>/skills/` | no |
| `<cwd>/.agents/skills/` | no |

`.agents/skills/` in a project directory is a documented location, but project
resources need trust, which a headless RPC session does not grant. An earlier probe
appeared to contradict this; it had simply not set `PI_CODING_AGENT_DIR`, so the agent
directory was somewhere else entirely.

Consequence: per-profile skills need an explicit `--skill <path>` on each Pi child
(the gateway already spawns one per profile with its own cwd and args), or a
project-level settings file, or they must be shared in the agent directory. The
`profiles/<name>/skills/` layout this file describes will load nothing until one of
those is done.

Emulating that surface is what keeps the Android app unchanged: it already
builds requests as `$serverUrl$profilePath/v1/...` and already sends
`X-Hermes-Session-Id`, so a gateway that honours both needs zero app changes.

## Conversation isolation is logical, not physical

Pi keeps one session file per conversation, and the gateway maps the app's
`X-Hermes-Session-Id` onto it — so two conversations never share a transcript, and
resuming one does not pull in the other's history. That part holds.

It is not a security boundary. The agent runs `bash` as root, and session files
live a couple of directories above its working directory:

```
/opt/pi/profiles/god      <- agent cwd
/opt/pi/sessions/...      <- ../../sessions, readable
```

Asked "what did I tell my other chat?", the agent can simply read the other
conversation's transcript off disk and answer from it. Observed during
verification: a second conversation correctly reported UNKNOWN when answering
from memory, then answered correctly with the first conversation's number once it
listed the sessions directory.

This is not new to the migration — the Hermes agent had the same reach
(`gateway/state.db` sat at `../../state.db` from a profile working directory) — but
it is worth stating plainly rather than letting "sessions are separate" imply an
isolation guarantee.

Closing it properly needs OS-level separation: a distinct user or container per
conversation, or sessions on a filesystem the agent cannot read. None of that is in
scope here, and it is worth deciding deliberately rather than discovering later.

## Base images are pinned by digest

Both `FROM` lines resolve through global `ARG`s holding a digest, so rebuilding
this file months later gives the same toolchain. They must stay **global**
(declared before the first `FROM`): podman resolves a stage-scoped `ARG` in a
later `FROM` to nothing and fails with a misleading `no FROM statement found`.
Verified by reproducing both layouts.

`golang` is build-time only — its binaries are copied out — so that digest
affects reproducibility, not the shipped runtime surface.

## Root-owned files on the host

The container runs as root against a workspace owned by host uid 10000, so
anything the agent writes into `/workspace` lands on the host **owned by root**.
That is expected and pre-existing — the gateway this replaces does the same —
but it surprises the first person who runs `chown` on the workspace expecting
uid 10000 and finds root-owned files.

To hand the tree back:

```sh
sudo chown -R 10000:10000 workspace/
```

Do that with the stack stopped. Doing it while a container is running races with
whatever the agent happens to be writing.

## Deferred, with the target written down

These were flagged in review on the runtime PR and are **not** addressed there.
Each has a named home so they are not quietly lost:

1. **Auth.** `gateway` gets `PASSWORD=${PASSWORD:-}`. Whatever serves `:8643`
   must require the same bearer token on both the direct port and the
   `/p/pi/*` route. An unauthenticated route to an agent with a shell is not
   acceptable.
2. **nginx prefix stripping.** `proxy_pass $pi/;` inside a `location` that uses
   a *variable* does **not** strip the prefix the way a static
   `proxy_pass http://pi/;` does — with variables nginx forwards the original
   URI. `/p/pi/v1/models` would arrive as `/p/pi/v1/models`. Needs an explicit
   `rewrite ^/p/pi(/.*)$ $1 break;` before `proxy_pass`.
3. **SSH mounts.** `docker/docker-compose.yml`'s `gateway` mounts the VPS
   keypair read-only and pins `GIT_SSH_COMMAND` for fail-closed agent-run git,
   because story/resumes push their repos. The Pi service needs the same three
   mounts plus the same `GIT_SSH_COMMAND`, or repo sync is broken.
4. **Healthcheck.** `pi mcp list` exits 0 with zero servers configured, so it
   proves nothing until `mcp.json` exists. Once it does, it is a genuine
   connectivity probe (it connects, it does not lint). A readiness check that
   actually dials the served port is still wanted on top.
5. **`depends_on`.** Use `condition: service_started` for the Pi service. A
   plain `depends_on` would make the proxy refuse to come up when Pi's MCP
   check fails, taking healthy `gateway` chat down with it.

## Tool selection is additive, never a list

Passing `--tools` on the CLI replaces the selection wholesale **and** filters
MCP tools out of the callable registry, which leaves `codemode` unable to reach
anything. `settings.json`'s `defaultTools` is additive. Measured: with
`--tools` passed, codemode reported no callable MCP tools; without it, the same
prompt correctly filtered 17 of 33 tasks in 9,497 tokens.

## git safe.directory is discovered, not hardcoded

Bind-mounted repos are owned by the host uid while the container runs as root,
so git rejects them as "dubious ownership" and every repo write fails.
`entrypoint.sh` walks `/workspace` at boot and registers each repo it finds.

A literal list in the Dockerfile was the first attempt and it is the wrong shape
for two reasons: the fourth bind-mounted repo breaks identically, and a path
baked into an image that no longer exists is indistinguishable from drift.
Runtime discovery also sidesteps the question of which pattern git honours —
measured on git 2.39.5, `/workspace/*`, `/workspace/**` and `/workspace/*/.git`
were all rejected while a literal path was accepted, so the pattern that "looks
right" may not be the one that works.

The scan is scoped to `/workspace`. A blanket `*` would exempt every directory
in the container, including paths with no business being exempted.

`test/Dockerfile` still carries the non-matching pattern; tracked in #216.
