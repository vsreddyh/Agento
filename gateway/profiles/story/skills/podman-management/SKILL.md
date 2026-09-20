---
name: podman-management
description: "Manage the project's Podman stack: container lifecycle, logs, health checks, and cleanup."
---

# Podman Management

## Services

- **LLM** — OpenCode direct (https://opencode.ai/zen/go/v1), no proxy. One `OPENCODE_API_KEY`, provider `opencode-go` only.

## Common commands

### Status
```bash
podman-compose -f docker/docker-compose.yml ps
podman ps --filter status=running
```

### Logs
```bash
podman-compose -f docker/docker-compose.yml logs gateway
podman-compose -f docker/docker-compose.yml logs -f --tail=50 gateway
```

### Restart single service
```bash
podman-compose -f docker/docker-compose.yml restart gateway
```

### Rebuild and start
```bash
podman-compose -f docker/docker-compose.yml up -d --build gateway
```

### Prune unused images after building
Dangling images pile up on every `--build`. Run this after building:
```bash
podman image prune -f
```
- Size check: `podman system df`

### Health check
```bash
curl -s https://opencode.ai/zen/go/v1/models -H "Authorization: Bearer $OPENCODE_API_KEY" | head -20
```

### Cleanup
```bash
podman-compose -f docker/docker-compose.yml down          # Stop + remove containers
podman-compose -f docker/docker-compose.yml down -v       # Also remove volumes
podman system prune -f --volumes                           # Full cleanup
```

## Pitfalls

- OpenCode direct — requires OPENCODE_API_KEY in .env
- Podman is daemonless — no `systemctl enable docker` equivalent needed.
- Rootless containers need lingering (`loginctl enable-linger $USER`, done by `init`) or cron jobs lose the user socket.
- After `.env` changes, restart the service: `podman-compose restart gateway`
