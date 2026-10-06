---
name: podman-management
description: "The project's Podman stack: what the agent can check from inside its own container, and the host-side commands for the human."
---

# Podman Management

**You run inside the `pi` container, not on the VPS.** `podman`,
`podman-compose`, `docker`, and `.env` exist only on the host. Every `podman`
command below is for the **human** — do not run them yourself.

## What you can check from in here

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8643/healthz
```

200 = gateway up. Nothing more. MCP calls succeeding = that server + MongoDB
reachable. `FATAL: one or more MCP servers failed to connect` in the pi log =
crash-loop, usually `MONGODB_URI` unset.

## Host-side (repo root on the VPS)

Services: `pi` (8643) · `health-api` (8001) · `proxy` (8080, app URL) ·
`retention` (one-shot). No `gateway` service.

```bash
podman-compose -f docker/docker-compose.yml ps
podman-compose -f docker/docker-compose.yml logs --tail 50 pi
podman-compose -f docker/docker-compose.yml run --rm retention --dry-run  # safe; real run deletes data
```

Retention prunes money > 90d, `hc_meals`/`hc_days` > 30d, never `hc_weight`.

## Talking to the gateway

Chat turns need `model_options.reasoning_effort` (required — an unrecognised
level is silently accepted, so a default would be indistinguishable). Wrong or
missing bearer token = `401`; malformed body = `400` naming the field. Host
only, from the repo root (`source .env` elsewhere silently sources nothing) —
there is no `.env` in-container.

## Rules

- Inspection safe, mutation not: `start|stop|restart|init|clean`, recreating
  containers, editing live `.env` — ask first, every time.
- After compose changes, `up -d` alone does not recreate: `ps`, `podman rm -f
  <container-name>`, `up -d pi`. Keep the explicit `-f`.
- `podman-compose` cannot parse `condition:` in `depends_on` — short list form only.
- Never `cat`/`echo` `PASSWORD` or `OPENCODE_API_KEY`, never commit them.
