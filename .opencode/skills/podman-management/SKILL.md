---
name: podman-management
description: "The project's Podman stack: health checks, compose commands, and the rules for touching the live VPS deployment."
---

# Podman Management

You run wherever opencode runs (dev checkout or VPS shell) — **not** inside
an agent container. There is no `pi` container anymore: the stack is
`health-api` + `proxy` + one-shot `retention`, all in
`docker/docker-compose.yml`, against MongoDB Atlas.

## What you can check read-only, any time

```bash
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8001/health
curl -s -o /dev/null -w '%{http_code}\n' http://127.0.0.1:8080/health
```

200 = that leg up. MCP calls succeeding = that server + MongoDB
reachable. `MONGODB_URI is not set` in a service log = the container got
no database, nothing works.

## Host-side (repo root on the VPS)

Services: `health-api` (8001) · `proxy` (8080, app URL) · `retention`
(one-shot). No agent service.

```bash
podman-compose -f docker/docker-compose.yml ps
podman-compose -f docker/docker-compose.yml logs --tail 50 health-api
podman-compose -f docker/docker-compose.yml run --rm retention --dry-run  # safe; real run deletes data
```

Retention prunes money > 90d, `hc_meals`/`hc_days` > 30d, never `hc_weight`.
`reports` (∃-Mail): read+unstarred pruned after 7d; unread/starred never touched.

## Rules

- Inspection safe, mutation not: `start|stop|restart|init|clean`, recreating
  containers, editing live `.env` — ask first, every time, and never do
  them unannounced: this checkout may BE the live stack.
- After compose changes, `up -d` alone does not recreate: `ps`, `podman rm -f
  <container-name>`, `up -d <service>`. Keep the explicit `-f`.
- `podman-compose` cannot parse `condition:` in `depends_on` — short list form only.
- Never `cat`/`echo` `PASSWORD` or `OPENCODE_API_KEY`, never commit them.
  All env lives in the single git-ignored root `.env`.
