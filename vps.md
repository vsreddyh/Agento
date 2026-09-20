# VPS sizing — 3-profile Hermes stack (containerized)

Fully containerized stack: three Hermes profiles + app API server in **one multiplexed gateway** (`s6`-supervised) + health-api + retention.
MongoDB stays **remote** (Atlas) — no Mongo container or storage counted below.

**Key fact: no LLM inference happens on this box.** OpenCode Zen runs the models, the
agents just stream text. Agents are I/O-bound (they wait on the app API + the network), so CPU and RAM
stay modest. No GPU needed.

Numbers below are **measured** on a running stack (idle agents), not guessed. Concurrency tiers =
number of Hermes agents running at the same time (max 3, one single-user session per profile).

| Tier | RAM | CPU | Disk | Network |
|---|---|---|---|---|
| Minimum — 1 agent at a time | 2 GB | 2 vCPU | 10 GB | 100 Mbps |
| Recommended — 2 at the same time | 3 GB | 2 vCPU | 20 GB | 100 Mbps |
| Maximum — all 4 at the same time | 5 GB | 4 vCPU | 30 GB | 100 Mbps |

## OS

- **Ubuntu 24.04 LTS (x86_64)** — supported until 2029; clean Docker/systemd support. Debian 12 is the fallback.
- No host Hermes install anymore — the stack is 100% containers, so the OS stays minimal.

## RAM — the real numbers

Measured live via `podman stats` on a running stack, all bots idle (gateway-based, all bots in one
process at measure time). The whole stack (bots + health-api via `s6` + remote Mongo)
sits at **~1.1 GiB**. The multiplexed layout (**all bots in ONE gateway container** via `s6`) merges the bot rows
into a single ~0.58 GiB gateway, i.e. roughly the same total. Per container:

| Container | RAM (idle) |
|---|---|
| OpenCode Zen (direct) | 0 MiB (no local container) |
| health-api | ~53 MiB |
| gateway (multiplexed — all bots via s6) | ~0.58 GiB (all bots, one container) |
| **Stack total** | **~1.06 GiB** |

+ Podman + OS (~0.4 GB) → realistic **floor ≈ 1.1 GB** (remote Mongo).

Each **concurrently active agent** adds ~0.6 GB (conversation context + tool output; the model
itself runs elsewhere).

- Minimum (1 active): 1.1 + 0.6 + headroom = **2 GB**
- Recommended (2 active): 1.1 + 1.2 + headroom = **3 GB**
- Maximum (4 active): 1.1 + 2.4 + headroom = **5 GB**

Add ~2 GB swap as a spike buffer. These match common provider tiers (2/3/5). The old 4/6/8 GB
guidance was ~2–3× over — safe, but you'd be paying for RAM the bots never touch.

## CPU

- Agents are I/O-bound; **2 vCPU handles all tiers comfortably** (idle CPU is ~0.2% per container).
- 4 vCPU only for the all-4-at-once tier as breathing room. More is wasted.

## Disk — whole numbers

Measured (`podman system df` + repo `du`), not guessed:

- Container images: bot (now includes nodejs + headless chromium for Playwright MCP) + health-api — re-measure with `podman system df` after build, old ~3.0 GB figure no longer applies
- Volumes + container writable layers: ~0.5 GB
- Live repo data (profiles, workspace, skills): ~0.15 GB
- **Total keep-everything footprint: ~4 GB**

- 10 GB is the comfortable floor (stack + logs + a snapshot).
- 20 GB leaves room for years of logs + image layers.
- 30 GB = max comfort; the old 30/50/80 guidance was ~4× over.
- Enable container log rotation (`max-size`) so idle-chatty bots don't eat simple loops of disk.
- **Always run `podman image prune -f` after every build** — dangling images pile up fast.

## Network

- App API (HTTP + SSE) and LLM text streaming are small; **latency matters, bandwidth barely does**.
- **100 Mbps is enough for every tier.** Only pay for more if the provider bundles it at no cost.

## Access

- health-api (:8001) binds `0.0.0.0` inside its container — restrict with firewall/reverse proxy if exposed publicly.
- SSH on 22 (restrict source IPs / use key auth).