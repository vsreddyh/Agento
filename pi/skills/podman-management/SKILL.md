---
name: podman-management
description: "The project's Podman stack: what the agent can check from inside its own container, and the host-side commands for the human. Use when asked whether the stack is up, or what to run on the VPS."
---

**Read this first: you are not on the host.**

You run **inside the `pi` container**, not on the VPS. Verified in the running
container:

| | in here | on the VPS host |
|---|---|---|
| working directory | `/workspace` | the repo root |
| `podman`, `podman-compose`, `docker` | **absent** | present |
| `docker/docker-compose.yml` | **absent** (only `pi/` and `workspace/` are bind-mounted) | present |
| `.env` file | **absent** — variables arrive from compose interpolation | present, git-ignored |
| `PASSWORD` | **already in your environment** | read from `.env` |
| `http://127.0.0.1:8643` | **you** — the pi-gateway in this container | pi, via the published port |
| `http://127.0.0.1:8080` | **connection refused** — the proxy is a different container | the proxy, the app's URL |

So: every `podman` command below is for the **human**. If you are asked about the
stack, check what you can actually check, report it, and quote the host command the
user would run. Do not try to run them — they will fail with "command not found", and
reporting that as a stack problem is worse than saying where the command belongs.

# Podman Management

## What you can check from in here

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8643/healthz   # 200 = you are serving
```

That is genuinely useful and genuinely limited: it tells you the gateway is up and
answering, and nothing about the other three services. For those you need the host.

Your MCP servers are the other thing you can see, and only by using them: if a
`miser-money` or `task-manager` call succeeds, that server is connected and MongoDB is
reachable. A call that fails with a backend error is not the same as a call that fails
to connect, and the difference is worth reporting precisely.

## Host-side commands (for the user — do not run these yourself)

Run from the repo root on the VPS, so `-f docker/docker-compose.yml` resolves. The
stack is four services:

| service | port | what it is |
|---|---|---|
| `pi` | `8643` | the agent host — one `pi` process per profile behind `pi-gateway` |
| `health-api` | `8001` | Android Health Connect sync into MongoDB |
| `proxy` | `8080` | nginx; the app's single URL (`/p/*` → pi, `/api/*` → health-api) |
| `retention` | — | one-shot data lifecycle, run by cron or on start |

There is no `gateway` service. If a command or document mentions one, it predates the
Pi migration and is wrong.

```bash
podman-compose -f docker/docker-compose.yml ps                    # what is running
podman-compose -f docker/docker-compose.yml logs --tail 50 pi    # a service's output
podman system df                                                  # disk usage
```

The pi entrypoint's own output is the fastest health signal: it registers the
bind-mounted repos with git, then gates startup on `pi mcp list` connecting all five
MCP servers. `FATAL: one or more MCP servers failed to connect` means the container is
crash-looping, and the line above names the server and the reason — usually
`MONGODB_URI is not set`.

## Ask before changing anything

Inspection is safe; mutation is not, on its own. Report what you find and stop — the
user decides whether the stack moves. This applies to you *especially*: you are root
inside a container whose restart you cannot see the end of.

Ask first, every time: `start`, `stop`, `restart`, `init`, `clean`, recreating any
container, `podman-compose up -d`, `image prune`, or anything that edits the live
`.env`. No surprise restarts — a restart under you mid-turn looks like your own work
failing.

### Two failure modes worth knowing before touching anything

- **`podman-compose up -d <service>` does not recreate a container whose config
  changed.** It reported success and left the old environment in place — not
  hypothetical: an `environment:` bug survived a successful `up` and the container ran
  with the wrong variables until it was removed by hand. After changing compose,
  verify the change landed, then recreate in three steps. Note the middle one takes a
  *container* name, which is project-prefixed and therefore not the service name:

  ```bash
  podman-compose -f docker/docker-compose.yml ps        # read the container name from here
  podman rm -f <container-name-from-ps>                 # …and use that exact string
  podman-compose -f docker/docker-compose.yml up -d pi
  ```

  The placeholder is deliberate: the project prefix on a container name comes from the
  directory name, so a literal example in a skill is one rename away from being wrong —
  including the one that stood here for two rounds.

  Keep the explicit `-f`: without it the command silently targets whatever compose file
  the current directory has, which on the host is only right from the repo root.

- **`podman-compose` cannot parse `condition:` in `depends_on`.** Any mapping value
  makes it die with `TypeError: cannot use 'dict' as a dict key`. Keep the short list
  form.

## Talking to the agent host from the host

```bash
# Host only, and only from the repo root — `source ./.env` elsewhere silently sources
# nothing and every call below answers 401:
#   set -a; source ./.env; set +a
# In-container there is no .env at all: compose interpolation already put PASSWORD in
# the environment.

curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8643/healthz   # unauthenticated by design
curl -s http://127.0.0.1:8080/api/model/options -H "Authorization: Bearer $PASSWORD"

# A real turn, cheapest model, no reasoning.
curl -sN http://127.0.0.1:8080/p/god/v1/chat/completions \
  -H "Authorization: Bearer $PASSWORD" -H 'Content-Type: application/json' \
  -d '{"provider":"opencode-go","model":"mimo-v2.6-flash",
       "model_options":{"reasoning_effort":"off"},
       "messages":[{"role":"user","content":"Reply with exactly: OK"}],"stream":true}'
```

`model_options.reasoning_effort` is **required**, not optional: Pi accepts an
unrecognised level silently, so a default would be indistinguishable from the
requested setting having been applied. A missing or wrong bearer token is `401`; a
malformed body is `400` naming the field.

## Credentials

`PASSWORD` and `OPENCODE_API_KEY` reach you as environment variables — never `cat` a
file for them, never `echo` one, and never let one into output that reaches your answer
or a commit. On the host they come from the git-ignored `.env`; in here compose has
already put them in your environment. If a diagnostic seems to need the value itself,
it almost certainly needs a redacted shape instead.

## Retention

One-shot, and it deletes data:

```bash
# Reports what it would remove, deletes nothing. Safe to run.
podman-compose -f docker/docker-compose.yml run --rm retention --dry-run
```

Without `--dry-run` it prunes money transactions over 90 days and `hc_meals` /
`hc_days` over 30 — `hc_weight` is never touched. Ask before running it for real.
