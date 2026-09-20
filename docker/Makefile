.PHONY: up down restart logs status setup build

COMPOSE := podman-compose --env-file ../.env -f docker-compose.yml

up:
	$(COMPOSE) up -d

down:
	$(COMPOSE) down

restart:
	$(COMPOSE) down && $(COMPOSE) up -d

logs:
	$(COMPOSE) logs -f

status:
	$(COMPOSE) ps

setup:
	@if [ -f ../.env ]; then echo ".env already exists — edit it to add your API keys"; else cp ../.env.example ../.env && echo "Created ../.env from .env.example — edit it before starting"; fi
	$(COMPOSE) up -d

build:
	$(COMPOSE) build --no-cache
