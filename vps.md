# VPS sizing — the Agento stack

A health/domain API, a reverse proxy and a one-shot retention job,
**fully containerized**. The agent does not run in a container any more
(#494): T3/opencode runs on the host and is a host tool's footprint, not a
container's, so it is not counted in the container totals below.

MongoDB stays **remote** (Atlas) — no Mongo container or storage counted below.

**Key fact: no LLM inference happens on this box.** OpenCode Go runs the models;
the agent streams text. It is I/O-bound (waiting on the API and the network), so
CPU and RAM stay modest. No GPU needed.

| Tier | RAM | CPU | Disk | Network |
|---|---|---|---|---|
| Minimum | 1 GB | 1 vCPU | 10 GB | 100 Mbps |
| Recommended | 2 GB | 2 vCPU | 20 GB | 100 Mbps |

## OS

- **Ubuntu 24.04 LTS (x86_64)** — supported until 2029; clean Podman/systemd support.
  Debian 12 is the fallback.
- No host agent install from this repo: the stack is 100% containers. T3/opencode
  is installed separately when the agent is wanted.

## RAM — the measured numbers

| Container | fresh start |
|---|---|
| `health-api` | ~16 MiB |
| `proxy` (nginx) | ~5 MiB |
| `retention` (one-shot, exits) | ~10 MiB while running |
| **Stack total** | **~25 MiB** |

The former `pi` container (~283 MiB fresh, ~353 MiB after a burst of turns) was
the reason this document once asked for 2/3/5 GB tiers; it no longer exists.

**OS + container runtime baseline ≈ 0.5 GB** — this one is an *allowance*, not a
measurement, because the host also carries other tooling. So:

- Minimum: 0.5 baseline + ~0.03 stack ≈ 0.53 GB — **1 GB** is comfortable for
  the container stack. Add T3/opencode's own footprint when the agent is running.
- Recommended: **2 GB** — buys headroom for builds and a host agent session.

Add ~2 GB swap as a spike buffer: the image build (`scripts/hermes.sh init`, i.e. `podman-compose --env-file .env -f docker/docker-compose.yml build` from the repo root) is
the hungriest operation in this repo, and building inside 1 GB of RAM with no
swap is how you get a killed build rather than a slow one. If you cannot spare
the swap, build elsewhere and pull.

## How to re-measure this

```bash
# NB podman stats reports a LIFETIME CPU average, so take the memory column as
# the reading and ignore CPU unless you sample twice.
podman stats --no-stream --format '{{.Name}}\t{{.MemUsage}}'
```

Disk and image figures come from `podman system df` and
`podman images --format '{{.Repository}}:{{.Tag}} {{.Size}}'`.

**Re-measure if the stack's contents change** — a new service, a bigger image or
a different retention workload changes the answer rather than the precision. A
number measured under different conditions is a guess with a date on it.

## CPU

- The stack is idle most of the time: `health-api` serves request-scoped work,
  nginx forwards it, and retention runs for seconds a day. One vCPU is enough;
  more is only useful for builds alongside the stack.
- With the agent in use, CPU stays low because inference happens at OpenCode Go;
  a turn is mostly a socket waiting on `opencode.ai`.

## Disk — whole numbers

Measured (`podman system df` + image sizes), not guessed:

| What | Size |
|---|---|
| `docker_health-api` image | ~24 MB |
| `docker_retention` image | ~18 MB |
| nginx image | ~20 MB |
| Volumes + container writable layers | < 1 MB |
| Live repo data (`workspace/` 215 MB, `exports/`) | ~0.22 GB |
| **Total, keep everything** | **~0.3 GB** |

- 10 GB is the comfortable floor (stack + logs + a snapshot).
- 20 GB leaves room for years of logs and image layers.
- **Always run `podman image prune -f` after every build** — dangling images pile
  up fast. `scripts/hermes.sh start` and `init` already prune; `restart`
  rebuilds without pruning, so prune manually after repeated restarts.
- Enable container log rotation (`max-size`) so the services do not quietly eat disk.

## Network

- Sync requests and mobile file transfers are small; **latency matters, bandwidth
  barely does**.
- **100 Mbps is enough.** Only pay for more if the provider bundles it at no cost.
- Outbound HTTPS to `opencode.ai`, MongoDB Atlas and GitHub.

## Access

- `proxy` (`:8080` compose default, `APP_PORT` remaps) is the app's single URL
  and the only port that needs to be reachable from a phone.
- `:8001` (health-api) stays published for direct access and debugging — restrict
  it with a firewall or reverse proxy if the host is public (`HEALTH_HOST_PORT`
  remaps the host side).
- `:8000` (or `$WORKSPACE_MCP_PORT`) is **not** published. It is the Gmail MCP
  OAuth callback, bound to `127.0.0.1` (loopback) by the host process T3 spawns —
  the launcher scrubs `WORKSPACE_MCP_HOST`/`WORKSPACE_MCP_BASE_URI`/
  `WORKSPACE_EXTERNAL_URL`, so this claim does not depend on the shell it starts
  in. It is reached only through an SSH forward during first-run consent
  (`ssh -L 8000:localhost:8000 <host>`, using the callback port on both sides).
  Never open it to the internet.
- SSH on 22 (restrict source IPs / use key auth).
