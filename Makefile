.PHONY: up down eval logs build test ps config

COMPOSE ?= docker compose
SERVICE ?= drydock

up:
	$(COMPOSE) up --build -d

down:
	$(COMPOSE) down --remove-orphans

logs:
	$(COMPOSE) logs -f $(SERVICE)

eval:
	DRYDOCK_MODE=eval $(COMPOSE) run --rm $(SERVICE)

build:
	go build ./...

test:
	go test ./...

ps:
	$(COMPOSE) ps

config:
	$(COMPOSE) config
