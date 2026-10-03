# `pi/` — the Pi agent dir

Mounted at `/opt/pi` as `PI_CODING_AGENT_DIR`. This is Pi's own state
directory, not application code. Everything here is committed except runtime
state (see `.gitignore`).

## Layout

| path | committed | purpose |
|---|---|---|
| `settings.json` | yes | Pi's own knobs — tool selection, retry, cache warming. **Not** personality. |
| `mcp.json` | yes *(later PR)* | the five in-repo Go MCP servers. Not in the runtime PR. |
| `profiles/<name>/AGENTS.md` | yes *(later PR)* | one per profile (god / story / resumes). The single source of truth for instructions, replacing every `SOUL.md`. |
| `profiles/<name>/skills/` | yes *(later PR)* | skills, loaded because each session's cwd is that profile dir. |
| `auth.json` | **no** | provider credentials. `OPENCODE_API_KEY` is injected by compose. |
| `models-store.json` | **no** | model catalog cache Pi refreshes at startup. |
| `sessions/` | **no** | session transcripts. |

Only `settings.json` exists so far. The three "later PR" rows are the intended
layout, not current contents — the runtime PR is the image plus agent-dir
skeleton; profile config and MCP wiring come after the gateway that serves them.

## Why no HTTP server here

Pi is a stdio process: `--mode rpc` speaks JSONL on stdin/stdout. The
OpenAI-compatible surface is a separate gateway binary that speaks RPC to Pi
over the container's stdio pair. It is what gets published as a port and routed
through nginx — not `pi` itself.

### What the gateway serves today

| route | served by the Pi gateway |
|---|---|
| `POST /{profile}/v1/chat/completions` | yes — SSE streaming, and a standard non-streamed completion when `stream` is false |
| `GET /healthz` | yes — unauthenticated liveness |
| `GET /{profile}/api/model/options` | **no** |
| `GET /api/model/options` | **no** |
| `GET /{profile}/v1/skills` | **no** |
| `GET /{profile}/v1/toolsets` | **no** |

The four unserved routes are not hypothetical: `ChatApi.fetchCatalog` and both
`ServerApi` inventory calls in the Android app request them today, and nginx
routes all of them to the gateway upstream (`docker/proxy/nginx.conf` sends
`/p/*` and both `/api/model/options` spellings there). Until they are served,
those app features are capability loss, and the proxy is still pointing at
Hermes for them.

Nothing in this list is launch-blocking: `/health` and `/api/files*` are served by
health-api through a different proxy location, `fetchCatalog` degrades to the app's
bundled offline provider list, and the two inventory pages would simply be empty.
It is still loss, so it is tracked rather than left to be discovered during the
Hermes removal.

`/v1/models` is deliberately absent from this table. Earlier drafts of this file
listed it because the Hermes API server exposed it; the app never calls it — it
builds its provider and model dropdowns from `/api/model/options` instead.

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
