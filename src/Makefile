# floatilla — run from src/
BUF     := runtime/agent/node_modules/.bin/buf
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: deps generate lint test build images up down logs cli clean-sandboxes

deps:
	cd runtime/agent && npm ci
	go mod download

generate: ## regenerate Go + TS code from proto/
	$(BUF) lint
	$(BUF) generate

lint:
	$(BUF) lint
	go vet ./...

test:
	go test ./...
	cd runtime/agent && npm test

build:
	go build ./...
	cd runtime/agent && npm run build

images:
	$(COMPOSE) build control sandbox

up: images
	$(COMPOSE) up -d
	@echo "control plane on http://localhost:7420 — try: go run ./cmd/floatilla types"

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f control launcher-plan launcher-impl launcher-deploy

cli:
	go build -o bin/floatilla ./cmd/floatilla

clean-sandboxes: ## remove leftover sandbox containers and workspace volumes
	-docker ps -aq --filter label=floatilla.managed=true | xargs -r docker rm -f
	-docker volume ls -q --filter name=floatilla-ws- | xargs -r docker volume rm
