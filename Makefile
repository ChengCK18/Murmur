# Common tasks. The Go toolchain is pinned to the local one (Go 1.25), since
# newer NATS and Prometheus modules would otherwise pull in Go 1.26.
export GOTOOLCHAIN := local
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: test itest web-test e2e failures infra up sitl down

test: ## unit tests; database and Redis tests skip if the infrastructure is not running
	go test -count=1 ./...

itest: infra ## the integration tests, with SITL if it is installed (about 4 minutes)
	go test -tags integration -count=1 -v ./integration/

web-test: ## the web app's type check and unit tests
	cd web && npm ci --no-fund --no-audit && npx tsc --noEmit && npx vitest run

e2e: ## browser tests against the running stack (make up first); restarts the gateway and fakegen
	cd web && npx playwright test --reporter=line

failures: ## the failure tests against the running stack (about 10 minutes); kills services under load
	rc=0; for t in kill_consumer restart_database kill_ingester kill_gateway drop_packets; do loadtest/failures/$$t.py || rc=1; done; exit $$rc

infra: ## NATS, TimescaleDB and Redis only
	$(COMPOSE) up -d --wait nats timescaledb redis

up: ## the whole stack: the map at http://localhost:8080, Grafana at :3000, 10 fake drones
	$(COMPOSE) up -d --build --wait

sitl: ## the whole stack plus 3 SITL drones; the first build compiles ArduPilot
	$(COMPOSE) --profile sitl up -d --build --wait

down:
	$(COMPOSE) --profile sitl down
