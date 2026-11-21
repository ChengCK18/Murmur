# Common tasks. The Go toolchain is pinned to the local one (Go 1.25), since
# newer NATS and Prometheus modules would otherwise pull in Go 1.26.
export GOTOOLCHAIN := local
COMPOSE := docker compose -f deploy/docker-compose.yml

.PHONY: test itest web-test e2e failures infra up sitl down kind-up kind-down k8s-up k8s-down

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

KIND_CLUSTER := murmur
IMAGES := ingester consumer commander alerter fakegen gateway

kind-up: ## a local Kubernetes cluster (kind) with Murmur's images built and loaded into it
	kind get clusters | grep -qx $(KIND_CLUSTER) || kind create cluster --config deploy/k8s/kind.yaml
	$(COMPOSE) build $(IMAGES)
	kind load docker-image --name $(KIND_CLUSTER) $(IMAGES:%=murmur/%:dev)

kind-down: ## delete the kind cluster
	kind delete cluster --name $(KIND_CLUSTER)

KUBE := --kube-context kind-$(KIND_CLUSTER)
HELM := helm $(KUBE)
KUBECTL := kubectl --context kind-$(KIND_CLUSTER)
MONITORING_CHART := oci://ghcr.io/prometheus-community/charts/kube-prometheus-stack
MONITORING_VERSION := 92.22.0
KEDA_CHART := https://kedacore.github.io/charts/keda-2.21.0.tgz

k8s-up: ## install or upgrade monitoring, KEDA and Murmur's chart on the kind cluster (make kind-up first)
	$(HELM) upgrade --install monitoring $(MONITORING_CHART) --version $(MONITORING_VERSION) \
		--namespace monitoring --create-namespace -f deploy/k8s/monitoring-values.yaml --wait --timeout 10m
	$(HELM) upgrade --install keda $(KEDA_CHART) --namespace keda --create-namespace --wait --timeout 5m
	$(KUBECTL) create configmap murmur-dashboards --namespace monitoring --from-file=deploy/grafana/dashboards \
		--dry-run=client -o yaml | $(KUBECTL) label --local -f - grafana_dashboard=1 -o yaml | $(KUBECTL) apply -f -
	$(HELM) upgrade --install murmur deploy/k8s/murmur --namespace murmur --create-namespace --reset-values --wait --timeout 5m

k8s-down: ## uninstall Murmur, KEDA and monitoring; the NATS and database volumes are kept
	$(HELM) uninstall murmur --namespace murmur --wait
	$(KUBECTL) delete configmap murmur-dashboards --namespace monitoring --ignore-not-found
	$(HELM) uninstall keda --namespace keda --wait
	$(HELM) uninstall monitoring --namespace monitoring --wait
