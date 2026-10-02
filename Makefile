.PHONY: up down logs rescan mixes smoke player test test-python test-go test-flutter build \
	dev-init dev-up dev-down dev-logs dev-player dev-worker dev-ps

COMPOSE ?= docker compose
DEV_COMPOSE ?= $(COMPOSE) -f docker-compose.yml -f docker-compose.dev.yml
BASE ?= http://127.0.0.1:8787

# Load .env if present (for TOKEN/PASSWORD in make targets)
ifneq (,$(wildcard .env))
include .env
export
endif

up:
	$(COMPOSE) up -d --build

down:
	$(COMPOSE) down

logs:
	$(COMPOSE) logs -f --tail=200

dev-init:
	$(DEV_COMPOSE) up -d --build

dev-up:
	$(DEV_COMPOSE) up -d --no-build

dev-down:
	$(DEV_COMPOSE) stop

dev-logs:
	$(DEV_COMPOSE) logs -f --tail=200

dev-player:
	$(DEV_COMPOSE) restart player

dev-worker:
	$(DEV_COMPOSE) restart worker

dev-ps:
	$(DEV_COMPOSE) ps

player:
	go -C player build -o bin/music-hive-player ./cmd/music-hive-player

test-go:
	go -C player test ./...

test-python:
	python -m pytest

test-flutter:
	cd mobile/flutter && flutter test

test: test-python test-go test-flutter

build: player

rescan:
	@test -n "$(MUSIC_HIVE_API_TOKEN)" || (echo "set MUSIC_HIVE_API_TOKEN" >&2; exit 1)
	curl -sS -X POST -H "Authorization: Bearer $(MUSIC_HIVE_API_TOKEN)" \
	  -H "Content-Type: application/json" -d '{}' \
	  $(BASE)/api/library/rescan

mixes:
	@test -n "$(MUSIC_HIVE_API_TOKEN)" || (echo "set MUSIC_HIVE_API_TOKEN" >&2; exit 1)
	curl -sS -X POST -H "Authorization: Bearer $(MUSIC_HIVE_API_TOKEN)" \
	  -H "Content-Type: application/json" -d '{}' \
	  $(BASE)/api/jobs/mix_pack

smoke:
	@chmod +x scripts/smoke_api.sh
	MUSIC_HIVE_BASE=$(BASE) MUSIC_HIVE_PASSWORD="$(MUSIC_HIVE_PASSWORD)" MUSIC_HIVE_API_TOKEN="$(MUSIC_HIVE_API_TOKEN)" \
	  ./scripts/smoke_api.sh

bench:
	@chmod +x scripts/bench_queue.sh
	./scripts/bench_queue.sh
