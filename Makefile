# Common tasks. The Go toolchain is pinned to the local one (Go 1.25), since
# newer NATS and Prometheus modules would otherwise pull in Go 1.26.
export GOTOOLCHAIN := local
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: test itest infra up down

test: ## unit tests; database and Redis tests skip if the infrastructure is not running
	go test -count=1 ./...

itest: infra ## the Phase 2 integration test (about 3 minutes)
	go test -tags integration -count=1 -v ./integration/

infra: ## NATS, TimescaleDB and Redis only
	$(COMPOSE) up -d --wait nats timescaledb redis

up: ## the whole stack, with 10 fake drones and Prometheus
	$(COMPOSE) up -d --build --wait

down:
	$(COMPOSE) down
