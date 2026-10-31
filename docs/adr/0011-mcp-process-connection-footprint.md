# ADR-0011: MCP process and Mongo connection footprint

- Status: proposed (2026-10)
- Issue: #424 (follow-up: #421, #423)

## Context

The deployment serves **two users**, and the container handles **one turn at a
time** per profile. Concurrency, throughput and headroom are non-goals: the only
wanted properties are correctness, a small footprint, and staying up. Any
dimensioning that assumes load is wrong on principle here.

Measured on the live stack 2026-10-31:

| Metric | Value | Notes |
|---|---|---|
| Processes in the pi container | 20 Mongo clients | `pi-gateway` (1) + `mcp-gate` relays (10) + MCP servers (9) |
| Established Mongo connections | 174 | 3 replica-set members → 3 mandatory monitor connections per client = **60 floor** |
| `maxPoolSize` | unset → **100 per client** | driver default; nothing sets it anywhere in the repo |
| Container memory (cgroup) | 350 MB | 479 MB summed RSS, ~130 MB shared |
| `mcp-gate` relays | 155 MB | **32% of memory for a second process per server** |
| `gmail` + `calendar` | 48 MB | for connectors that currently answer 501 (no OAuth configured) |
| Go GC | default (`GOGC=100`) | 19 processes, each holding ~2× live heap |

`AGENTS.md` and `README.md` claim 1.2–1.8 GB steady state. The real figure is
350 MB; that claim is stale and should not be trusted for sizing.

Two further facts bound the decision space:

1. **The connection count did not cause the outage.** The 2026-10-31 outage was
   774 restarts cycling 20 pools at a time (#423), which exhausted the cluster.
   Steady-state 174 connections is uncomfortable, not fatal. Removing churn is
   mandatory regardless of what happens here.
2. **The approval relay is not a hard security boundary.** `pi/settings.json`
   exposes `bash`, the agent runs as root, and `/usr/local/bin/miser-money` is
   world-executable — so the agent can already run a server binary directly and
   skip `mcp-gate` entirely. The relay enforces the *MCP path Pi spawns*; it
   defends against the model calling a gated tool, not against a determined
   process. ADR-0002 assumed a stronger boundary than the image actually has.

## Decision

Adopt the smallest-footprint design that keeps current behaviour, in three
stages, each independently shippable and measurable.

### Stage 1 — stop paying for scale we do not have (no architecture change)

- Set `maxPoolSize=2`, `minPoolSize=0` **as code defaults** in
  `internal/mongo`, not only in `.env`: a pool ceiling that lives in an
  untracked file drifts. At two users, a floor of 60 monitors and a ceiling of
  100 pooled connections per client is dimensioning for a load that will never
  arrive.
- Set `GOGC=50` for Go services. 19 processes holding 2× heap headroom is the
  single cheapest memory win available.
- Do not wire `gmail`/`calendar` into `pi/mcp.json` until OAuth is actually
  configured. 48 MB and 12 connections for tools that answer 501 is waste, and
  #421 already made their absence survivable.

### Stage 2 — approval gate as a library, delete the relay

Replace `cmd/mcp-gate` with a shared wrapper applied where each server registers
its tools (the same `approvals.NeedsApproval` / audit / hold-poll logic, minus
the JSON-RPC re-parsing). `pi/mcp.json` points straight at each server.

Removes 10 processes, 10 Mongo clients and ~155 MB, and takes 174 connections to
roughly 40. It also *simplifies* the code: the relay currently re-encodes every
frame in both directions to intercept `tools/call`.

This is accepted despite losing process isolation for enforcement, because of
Context fact 2: the boundary is already soft, and `bash` bypasses it today.
Trading a boundary that is not real for 155 MB and 70 connections is a good
trade. It must be recorded as defense-in-depth, never described as a sandbox.

### Stage 3 — multi-call binary (only if measurements still justify it)

Compile gate + all nine servers into one `agento-mcp` binary
(`agento-mcp --server gmail`), so 9 processes share one text segment instead of
mapping ~11 MB each privately. Estimated −60–90 MB. Deferred: it is pure
memory, it changes every `cmd/*Main`, and Stage 2 may make it unnecessary. It
does **not** reduce process count or connections by itself.

## Considered options

- **Status quo** — rejected: three processes where one suffices, for a
  load-bearing ceiling of 100 connections per client at two users.
- **Multi-call binary alone (Stage 3 without Stage 2)** — kept as Stage 3:
  memory only, leaves 2 processes per server and all 20 Mongo clients.
- **Relay queries the gateway's Mongo over HTTP** — rejected: it preserves the
  process boundary but adds a network hop inside the approval path and a new
  internal auth surface, i.e. more moving parts than deleting the relay.
- **`directConnection=true`** — rejected: it cuts monitors from 60 to 20 at the
  cost of failover awareness. Stage 2 removes most of those monitors anyway, so
  the trade buys little and costs real robustness.
- **Fold the gate in-process but keep 20 processes** — incoherent: pays the
  relay's memory without its isolation.

## Consequences

- Enforcement moves from a process boundary to a shared library. ADR-0002's
  claim to "enforce in a relay" is superseded in substance; the audit and
  approval *behaviour* is unchanged, the isolation is gone. This ADR should be
  cited where that guarantee is described.
- Pool caps must be verified not to serialize tool calls. With one turn at a
  time this is expected to be safe, but it is a behaviour change and needs a
  test rather than an assumption.
- `AGENTS.md` / `README.md` memory guidance must be corrected once Stage 1
  lands, or the next incident is sized against a number that was never true.
- `#423` (retry + backoff) stays mandatory and is independent: none of these
  stages prevents a restart storm.

## Verification

Same commands before and after each stage, so the numbers are comparable:

```bash
podman stats --no-stream --format "{{.Name}} {{.MemUsage}}"          # cgroup
podman exec docker_pi_1 sh -c 'ps -eo rss,comm --sort=-rss | head'   # per-process
podman exec docker_pi_1 sh -c 'cat /proc/net/tcp /proc/net/tcp6 |
  awk "\$3 ~ /:6989\$/ && \$4 == \"01\" {n++} END {print n}"'         # connections (:6989 = 27017)
```

Targets: ≤10 Mongo clients, ≤60 connections at idle, ≤200 MB cgroup, and
`pi` alone (Node) remaining the largest single consumer.
