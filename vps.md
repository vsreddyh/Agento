# VPS sizing — the Pi stack

Three profiles (god, story, resumes) plus a health-sync API, a reverse proxy and a
one-shot retention job, **fully containerized**. The agent host is **one `pi`
container** running **one `pi` process per profile**, fronted by `pi-gateway`.

MongoDB stays **remote** (Atlas) — no Mongo container or storage counted below.

**Key fact: no LLM inference happens on this box.** OpenCode Go runs the models; the
agents stream text. They are I/O-bound (waiting on the app API and the network), so
CPU and RAM stay modest. No GPU needed.

Numbers below are **measured** on the running stack, not guessed. Measured
2026-10-04 on this host, `mimo-v2.6-flash` with `reasoning_effort: off`, three
profiles discovered, five MCP servers connected.

| Tier | RAM | CPU | Disk | Network |
|---|---|---|---|---|
| Minimum — 1 profile at a time | 1 GB | 2 vCPU | 10 GB | 100 Mbps |
| Recommended — 2 at once | 2 GB | 2 vCPU | 20 GB | 100 Mbps |
| Maximum — all 3 at once | 3 GB | 2 vCPU | 30 GB | 100 Mbps |

The three-at-once row is not an estimate: all three profiles were run concurrently on
**2 vCPU** while measuring the table below, and all three answered. The 3 GB figure is
headroom for image builds, not for the agents — see [CPU](#cpu).

## OS

- **Ubuntu 24.04 LTS (x86_64)** — supported until 2029; clean Podman/systemd support.
  Debian 12 is the fallback.
- No host agent install: the stack is 100% containers, so the OS stays minimal.

## RAM — the measured numbers

| Container | fresh start | after 1 turn | after 3 turns at once |
|---|---|---|---|
| `pi` (three profiles + `pi-gateway` + five MCP servers) | ~283 MiB | ~320 MiB | ~353 MiB |
| `health-api` | ~16 MiB | ~16 MiB | ~17 MiB |
| `proxy` (nginx) | ~5 MiB | ~5 MiB | ~5 MiB |
| **Stack total** | **~305 MiB** | **~341 MiB** | **~375 MiB** |

**Size for the right-hand column, not the left.** `pi` does not give the memory back:
after a burst of three concurrent turns it sat at **353.3 / 353.4 / 353.0 MiB across
three consecutive minutes**, flat. The 283 MiB figure is what a freshly started container
looks like before it has ever served a turn, and a box provisioned against it will be
smaller than the box actually in use. Steady state for a stack anyone is really using is
**~375 MiB**.

That flatness is the good news. **Three concurrent turns cost 70 MiB more than a fresh
start** — roughly 23 MiB each, with the first turn the most expensive of the three. The
reason is structural: no inference runs here, so a turn is mostly a socket waiting on
`opencode.ai`.

**The correction that matters most:** the previous version of this document put each
concurrent turn at **~0.6 GB** and sized its tiers from that (1.1 + 0.6 → 2 GB). The
measured figure is **~23 MiB** — a difference of ~25×, and it is what drove every tier in
the old table. It was an estimate written as though it were a reading.

**OS + container runtime baseline ≈ 0.5 GB** — this one is an *allowance*, not a
measurement. This host cannot give a reading for it: 4.8 GiB total with ~2.7 GiB in use
while the stack itself accounts for 0.3 GiB, so the rest is other tooling and agent
sessions sharing the box. The figure below is the container total (measured) plus that
allowance.

So, from the measured totals:

- Minimum (1 active): 0.5 baseline + 0.375 steady state ≈ 0.9 GB — **1 GB is genuinely
  tight**, no headroom at all. It works for a stack that only ever serves one profile.
- Recommended (2 active): **2 GB** — the step up buys headroom, not agent capacity
- Maximum (3 active): measured use is still ~0.9 GB, so **3 GB is for builds**, not turns

That is a large drop from the previous guidance, which asked for 2/3/5 GB against a
~1.06 GiB measured stack. The reason is structural, not tuning: the agent host is now one
Node container with three `pi` processes, where it used to be a Python-runtime image with
a bundled headless browser. **If your workload starts using the browser toolset, re-measure** — nothing in
this stack ships chromium any more, so a browser-using turn will cost more than the
figures above and the delta is not something this document can predict.

Add ~2 GB swap as a spike buffer — and on the **1 GB minimum tier that swap is
effectively required, not optional**: `podman-compose build` is the hungriest operation
in this repo (911 MB image plus a Go build stage), and building inside 1 GB of RAM with
no swap is how you get a killed build rather than a slow one. If you cannot spare 2 GB of
swap on a 1 GB box, build elsewhere and pull.

These match common provider tiers (1/2/3). The old
2/3/5 GB guidance was ~2× over — safe, but you would be paying for RAM the agents never
touched.

Two other numbers that changed and should be flagged rather than quietly replaced: the old
disk table assumed the agent image carried **headless chromium for Playwright** (that was
~3.0 GB of image on its own; the current image is 911 MB and ships no browser at all), and
it put volumes and writable layers at ~0.5 GB where they now measure **under 1 MB**.

## How to re-measure this

The figures above are only as good as the method that produced them, so here it is. Run
these against the running stack and you should land within a few MiB of the table.

```bash
# Fresh start: restart the stack first, or the number is really the steady-state one.
# NB podman stats reports a LIFETIME CPU average, so take the memory column as the reading
# and ignore CPU unless you sample twice.
podman stats --no-stream --format '{{.Name}}\t{{.MemUsage}}' 

# One turn in flight: stream a completion and sample while it is open.
curl -sN http://127.0.0.1:8080/p/god/v1/chat/completions \
  -H "Authorization: Bearer $PASSWORD" -H 'Content-Type: application/json' \
  -d '{"provider":"opencode-go","model":"mimo-v2.6-flash",
       "model_options":{"reasoning_effort":"off"},
       "messages":[{"role":"user","content":"Reply with exactly: god"}],"stream":true}' \
  > /dev/null &
sleep 15 && podman stats --no-stream --format '{{.Name}}\t{{.MemUsage}}'

# Three at once: same request with /p/story and /p/resumes as well, then sample.
# Verify all three answered before trusting the number.
```

Disk and image figures come from `podman system df` and
`podman images --format '{{.Repository}}:{{.Tag}} {{.Size}}'`.

**Re-measure if any of these change**, because each one changes the answer rather than
the precision: the model (a larger context window costs more), `reasoning_effort`, the
agent image's contents (chromium is the big one — see below), the number of MCP servers,
or the profile count. A number measured under different conditions is a guess with a
date on it.

## CPU

- Agents are I/O-bound, and this is verified rather than argued: with **three turns in
  flight** the host's 1-minute load average was **0.77 on 2 vCPU**. The agents were not
  the reason there was headroom.
- A caveat on reading CPU off `podman stats`: its `CPU%` is an **average over the
  container's lifetime**, not an instantaneous reading — three consecutive `--no-stream`
  samples returned the same 3.46% for `pi` while it was genuinely doing different work.
  The lifetime averages for this stack are `pi` ~3.5%, `health-api` ~1.2%, `proxy` ~0.9%,
  which say the boxes are not busy but say nothing about a given instant.
- More vCPU is wasted unless you run builds alongside the stack. `podman-compose build` of
  the agent image (Node + Pi + a Go stage) is the only genuinely CPU- and memory-hungry
  operation here — that, not the agents, is what the 3 GB maximum tier is for.

## Disk — whole numbers

Measured (`podman system df` + image sizes), not guessed:

| What | Size |
|---|---|
| `docker_pi` image (Node + Pi + Go MCP servers + tectonic) | ~911 MB |
| `docker_health-api` image | ~24 MB |
| `docker_retention` image | ~18 MB |
| Volumes + container writable layers | < 1 MB (measured: 36 kB + 682 kB) |
| Live repo data (`pi/` 0.5 MB, `workspace/` 215 MB, session transcripts) | ~0.22 GB, growing with transcripts |
| **Total, keep everything** | **~1.2 GB** |

- 10 GB is the comfortable floor (stack + logs + a snapshot).
- 20 GB leaves room for years of logs, image layers and session transcripts.
- **Always run `podman image prune -f` after every build** — dangling images pile up fast.
  This host reached **18.8 GB across 421 images**, almost all of it reclaimable, purely
  from stale build tags; the stack itself needs ~1 GB.
- Enable container log rotation (`max-size`) so idle agents do not quietly eat disk.

## Network

- Chat (HTTP + SSE) and LLM text streaming are small; **latency matters, bandwidth
  barely does**.
- **100 Mbps is enough for every tier.** Only pay for more if the provider bundles it at
  no cost.
- Outbound HTTPS to `opencode.ai`, MongoDB Atlas and GitHub (story/resumes push their
  own repos over SSH).

## Access

- `proxy` (:8080) is the app's single URL and the only port that needs to be reachable
  from a phone.
- `:8643` (pi) and `:8001` (health-api) stay published for direct access and debugging —
  restrict them with a firewall or reverse proxy if the host is public.
- SSH on 22 (restrict source IPs / use key auth).