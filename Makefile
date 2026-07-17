.PHONY: up down eval logs build test test-nocgo vet ci ps config

COMPOSE ?= docker compose
SERVICE ?= drydock-core

up:
	$(COMPOSE) up --build -d

down:
	$(COMPOSE) down --remove-orphans

logs:
	$(COMPOSE) logs -f $(SERVICE)

eval:
	DRYDOCK_MODE=eval $(COMPOSE) run --rm $(SERVICE)

build:
	CGO_ENABLED=1 go build ./...

vet:
	CGO_ENABLED=1 go vet ./...

test:
	CGO_ENABLED=1 go test -count=1 ./...

test-nocgo:
	CGO_ENABLED=0 go test -count=1 ./internal/symbols/...

ci: vet build test

ps:
	$(COMPOSE) ps

config:
	$(COMPOSE) config
