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
| `profiles/<name>/skills/` | yes | Per-profile skills. **Not discovered as laid out** — see the profile-skills finding below; loaded by an explicit `--skill` path per profile. |
| `extensions/` | yes | Pi extensions loaded by every profile. Currently one: `subagent.ts`, the `spawn_subagent` delegation tool. See the subagent section below. |
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

## The five deferred items, and where each one landed

All five were flagged in review on the runtime PR. Four landed with the `pi`
compose service; the fifth landed with the cutover.

| item | status |
|---|---|
| **Auth** | done. The `pi` service takes `PASSWORD=${PASSWORD:-}`, the same value the Hermes gateway did, so a tab can be repointed without re-entering it. Verified: both a missing and a wrong bearer token answer 401. |
| **SSH mounts** | done. Same three keypair mounts and the same fail-closed `GIT_SSH_COMMAND` as `gateway`, so story/resumes push their repos unchanged. |
| **Healthcheck** | done. `curl -fsS http://localhost:8643/healthz` inside the container, with a 60s `start_period` because the entrypoint connects all five MCP servers first. The entrypoint's `pi mcp list` is the connectivity gate; this is the port check on top of it. |
| **`depends_on`** | done, in the short list form (`- pi`). The long form is not available on this stack at all: podman-compose builds its dependency graph with `deps = {x: {} for x in deps}`, so any `condition:` value is an unhashable dict and it dies with `TypeError: cannot use 'dict' as a dict key` before reading a service. Verified in both spellings. The short form still means *start pi first* — podman-compose's own `config` output renders it as `condition: service_started` — and ordering is all this needs, because nginx re-resolves `pi` per request. Deliberately **not** gated on pi's *health*: that healthcheck fails whenever its MCP check does, and a proxy held back by it would take health-api's `/api/*` routes down too — health sync has nothing to do with the agent. |
| **nginx prefix stripping** | done, in the cutover PR. `rewrite ^/p(/.*)$ $1 break;` before `proxy_pass $chat`, and it is load-bearing rather than tidying. Measured against nginx 1.27 with echo upstreams: with the rewrite `/p/god/v1/chat/completions` arrives as `/god/v1/chat/completions`; without it the original path arrives unchanged, and pi-gateway reads the first segment `p` as a profile name and 404s. |

The unprefixed `/api/model/options` needs no rewrite: pi-gateway serves that path
itself, with `api` a reserved first segment rather than a profile name.

## What the cutover changed, and what is left of Hermes

The cutover is one line plus a rewrite — `set $chat http://pi:8643` — and it is
reversible by changing it back. Both stacks stay in the compose file until the
Hermes removal, so a tab can be pointed at either port to compare.

The proxy's read/send timeout went from 300s to **960s** in the same change, and
that is not cosmetic: a `spawn_subagent` delegation is capped at 900s of work plus
up to 30s of teardown, so a turn that legitimately spends that budget would be cut
mid-answer at 300s while the agent kept working. 960s = 900 + 30 + 30s of margin.
It is an idle timeout between bytes, not a total-turn limit.

Still on Hermes, and still running:

- the `gateway` service, `test/Dockerfile`, `test/entrypoint.sh`, and the nine
  tracked files under `gateway/` (`SOUL.md` x3, `config.yaml.template` x3,
  `podman-management/SKILL.md` x2, and a `.curator_state` that should never have
  been tracked). Its untracked runtime state on this host stays where it is.
- `scripts/hermes.sh`, which still builds the Hermes bot image and reports
  "gateway (3 profiles)".
- `README.md`, `documentation.md` and the root `AGENTS.md`, which all describe the
  Hermes gateway as the live chat path.

Two parity questions the cutover does not answer, both of which need deciding
rather than code:

- `podman-management` exists for two Hermes profiles with no Pi equivalent.
- `SOUL.md` personality (god 44 / story 59 / resumes 61 lines) has no Pi
  counterpart file. It appears to have been folded into `pi/profiles/*/AGENTS.md`,
  but nothing here says so, and Pi has no `SOUL` concept of its own — so "the
  personality survived" is currently an assumption, not a verified fact.

## Subagent spawning is a separate process, and that is the whole point

`extensions/subagent.ts` registers `spawn_subagent`: run an independent agent to
completion on a self-contained task and return its answer. This is the delegation
the Hermes profiles had.

The subagent is a **separate `pi` process** driven over RPC, not an in-process
`createAgentSession`. That is not a style preference; the in-process route leaks.

An SDK session does not load the CLI's built-in extensions, so reproducing MCP
means hand-building a `DefaultResourceLoader`. That does work — 43 MCP tools,
measured — but the child then owns five MCP server subprocesses that nothing ever
reclaims:

| step | result |
|---|---|
| `AgentSession.dispose()` | does **not** emit `session_shutdown` |
| MCP extension closes connections | **only** on `session_shutdown` |
| `AgentSessionRuntime.dispose()` | does emit it — but does not reproduce the MCP wiring (0 tools) |
| `session.extensionRunnerRef.current` | **unpopulated** for SDK sessions, so the event cannot be raised by hand |
| shared loader across children | corrupts MCP: 43 tools → 23, servers dying |

Measured cost of the in-process route: **five processes stranded per delegation**,
accumulating for the life of the container. A process is reaped by
`RpcClient.stop()`, which SIGTERMs the CLI and the CLI closes its own MCP clients on
the way out — **0 processes left behind**, measured across repeated delegations.

Sharing one loader to get "only one connection set" was tried and is wrong: it drops
the child to 23 tools and servers die. One loader per child is correct.

What the child's flags buy, each structural rather than checked at runtime:

| flag | why |
|---|---|
| `--no-extensions` | the child loads no extension, **including this one**, so `spawn_subagent` does not exist in its world and it cannot recurse. A depth counter is a limit a bug could step past; an absent tool cannot be called. |
| `-e builtin:mcp -e builtin:codemode -e builtin:tool-search` | `--no-extensions` disables the built-in extensions too, so they are re-enabled by name. Without this the child has no money/cookbook/health/task/project tools and delegation is useless. Measured: 50 tools, 43 of them MCP. |
| `--no-session` | no session file per delegation for the gateway's session store — which has never heard of these conversations — to accumulate. |
| forwarded `--skill <path>` | read from this process's own `argv`, so per-profile skills reach the child. |

Cost is **not** guessed from the stream: `getSessionStats()` is authoritative.
Per-delta usage repeats within one message, so summing streamed frames multiplies a
single turn's cost. That figure is folded into the tool result's own `usage`, because
Pi adds nested result usage to the caller totals — otherwise delegation looks nearly
free while being one of the most expensive things the agent can do. Stats are fetched
**best-effort**: a failure there sets `details.usageUnavailable` rather than throwing,
because discarding an answer the parent already paid six processes for is the worst
available outcome, and the natural response — retry — pays that cost again.

### What the tool validates before it spawns anything

Every model-controlled value is checked *before* a process exists, so a bad value is
refused where the parent can see what it asked for rather than as a child that dies
during startup:

| input | rule | why |
|---|---|---|
| `task` | must be non-empty and <= 20k characters | output is truncated on the way back, so input is bounded for symmetry; 20k chars (~5k tokens) is generous, and the error tells the caller to name files rather than paste them |
| `timeoutSeconds` | must be finite and >= 1s, capped at 900s | a subagent pins six processes for as long as it runs, so a sub-second timeout would pay the full spawn cost to expire immediately; `0` silently becoming the default would hide the mistake |
| `thinking` | an explicitly requested level must be one of `off, minimal, low, medium, high, xhigh, max` | Pi **silently accepts an unknown level**, so a typo would leave the child at a depth nobody chose. The **inherited** level is passed through unvalidated on purpose — it came from a working parent, and checking it would mean a new Pi level breaks delegation for every profile using it |
| `cwd` | must be non-empty, exist, and be a directory; a relative path resolves against this agent's directory | a typo guard, **not** a security boundary — the caller already holds `bash` and full filesystem access. `""` is a typo, not an inheritance request |
| `model` | trimmed, must name a model, resolved against the parent's registry | the CLI resolves lazily, so an unknown id would otherwise fail on the child's first turn; `undefined` inherits but `""` is a typo, and `provider/` with no model must not reach the registry |

`off` is deliberately in the thinking whitelist even though it is absent from Pi's
`ThinkingLevel` union — it lives in `ModelThinkingLevel`, and it is the value every
profile here actually runs (`muse-spark-1.3-contributor:off`). A whitelist built from
the declared type alone would reject the common case.

### Startup is inside the timeout, and the answer never grows unbounded

Three things that looked fine and were not, all the same class of bug: a value that
is bounded on the way *out* but unbounded on the way *in*, or a wait nobody is
holding.

| thing | before | after |
|---|---|---|
| `client.start()` | unbounded — `timeoutMs` covered only `promptAndWait`, so a child stalling while connecting MCP sat there past `timeoutSeconds` with six processes pinned | raced against the same `timeoutMs` by `within()`, which clears its timer in `.finally`. Losing the race costs nothing but the wait: the caller's `finally` still stops the child |
| `answerOf` | joined every `text_delta` first and truncated the result, so a runaway child could bloat the parent before the cut | accumulation stops at `2 × MAX_RESULT_CHARS`. The overshoot is deliberate — it leaves `truncate` enough text to report how many characters were actually dropped instead of a made-up number |
| `cliPath()` | returned `process.argv[1]` as-is; a relative entry passes `existsSync` here and then resolves against the child's cwd | returned absolute. Same bug class as the relative `cwd` and relative `--skill` cases above: verified here, wrong there, no error |

The cap is applied to the *append* rather than only the loop condition: a child that
emits one enormous delta would pass a between-events check and overshoot by its
whole size in a single step, which is exactly the case the cap exists for.

Three smaller coercions, all the same lesson — a guard that reports the wrong thing
is worse than no guard, because the parent trusts it:

| value | trap | fix |
|---|---|---|
| `timeoutSeconds: NaN` | `JSON.stringify(NaN)` is `"null"`, so the error said the caller passed `null` | `String(seconds)`, which prints `NaN` |
| `modelRegistry.find()` | `=== undefined` treats a `null` "not found" as a real model, and the child fails on its first turn instead | falsy check — `find` is third-party |
| session stats | `Number.isFinite(-1)` is true, so a negative count would subtract from every total it is added to | `Math.max(0, …)` in `count()` |

### One ceiling for the whole call, not one per phase

`timeoutSeconds` bounds *the delegation*, not each of its phases. Startup and the
prompt share one budget: the prompt is given `timeoutMs - (elapsed)`, so a child
that takes 14 minutes to start still cannot push the call past the 15 minutes the
caller asked to be capped at. Granting both phases the full `timeoutMs` would have
made the advertised "hard ceiling" worth double, which is the same resource claim
the ceiling exists to make. If startup has already consumed the budget the call
fails immediately rather than prompting with a negative allowance.

The validators are also total functions: `task`, `cwd`, `model` and `thinking` are
taken as `unknown` and `typeof`-checked, so a wrong-typed value gets the message that
names the field instead of a `TypeError` from `.trim()` that names nothing. The
schema already enforces these types, so this is defence for a caller that does not.

### Teardown is bounded, and a leak leaves a trace

The `finally` that calls `client.stop()` is the one wait with no budget behind it:
the startup and prompt deadlines were both inside `timeoutSeconds`, but a child
stalling while closing its MCP servers would pin all six processes indefinitely,
with nothing left waiting on them. `stop()` is therefore raced against its own
30s ceiling — deliberately outside `MAX_TIMEOUT_MS`, because by then the call has
already failed or answered and there is no delegation budget left to spend.

Its failure is logged rather than swallowed silently. Swallowing is right for the
error that *explains* the failure (the original must not be replaced by a teardown
complaint), but this is the one path where the processes may genuinely still be
running, and a leak with no trace is what makes the next occurrence undebuggable.

The tool's own `timeoutSeconds` description says `plus up to 30s to tear the child
down afterwards`, so the contract the model reads is the contract the code has.

`resolveCwd` maps **only** `ENOENT`/`ENOTDIR` to "no such directory". `EACCES`,
`EPERM` and `ELOOP` mean the path is there and something else is wrong; reporting
those as missing sends the caller to fix a path that exists, so they are re-thrown
with the real code (`cannot use cwd <path>: EACCES`).

### One at a time, not in parallel

Each delegation is one `pi` process plus five MCP servers, so parallel calls fan out
linearly. The tool description says to call them serially rather than shipping a
semaphore: a hard cap would throttle legitimate parallel work, and Pi already runs a
tool batch in order by default. This is a prompt-level guard, not an enforced one —
worth revisiting if the model ever starts fanning out on its own.

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
