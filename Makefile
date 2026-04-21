.PHONY: up down eval logs build test test-cgo vet ci ps config

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
	CGO_ENABLED=0 go build ./...

vet:
	CGO_ENABLED=0 go vet ./...

test:
	CGO_ENABLED=0 go test -count=1 ./...

test-cgo:
	CGO_ENABLED=1 go test -count=1 ./...

ci: vet build test

ps:
	$(COMPOSE) ps

config:
	$(COMPOSE) config
