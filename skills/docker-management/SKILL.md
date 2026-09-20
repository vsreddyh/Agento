---
name: docker-management
description: "Manage the project's Docker stack: container lifecycle, logs, health checks, and cleanup."
---

# Docker Management

## Services

- **LLM** — OpenCode direct (https://opencode.ai/zen/v1), no proxy. One `OPENCODE_API_KEY` covers `opencode` + `opencode-go`.

## Common commands

### Status
```bash
docker compose -f docker/docker-compose.yml ps
docker compose -f docker/docker-compose.yml ps --status running
```

### Logs
```bash
docker compose -f docker/docker-compose.yml logs gateway
docker compose -f docker/docker-compose.yml logs -f --tail=50 gateway
```

### Restart single service
```bash
docker compose -f docker/docker-compose.yml restart gateway
```

### Rebuild and start
```bash
docker compose -f docker/docker-compose.yml up -d --build gateway
```

### Always clean the build cache after building
Build cache grows fast (7.6 GB on this box) and never shrinks on its own.
Run this after every `--build` / `up -d --build`:
```bash
docker builder prune -f        # drop dangling build cache
```
- Run it **every build**, not occasionally.
- `-f` = no prompt. Safe — only removes cached layers, never images/containers.
- Size check: `docker system df`

### Health check
```bash
curl -s https://opencode.ai/zen/v1/models -H "Authorization: Bearer $OPENCODE_API_KEY" | head -20
```

### Cleanup
```bash
docker compose -f docker/docker-compose.yml down          # Stop + remove containers
docker compose -f docker/docker-compose.yml down -v       # Also remove volumes
docker system prune -f --volumes                           # Full cleanup
```

## Pitfalls

- OpenCode direct — requires OPENCODE_API_KEY in .env
- After `.env` changes, restart the service: `docker compose restart gateway`
