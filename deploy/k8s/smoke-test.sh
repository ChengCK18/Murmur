#!/bin/sh
# Checks a running Murmur on the kind cluster (make k8s-smoke, after
# make kind-up k8s-up): that NATS formed one cluster holding three copies of
# the stream, telemetry from the fake drones reaches the database, the map
# and Grafana answer on the host, Prometheus scrapes every Murmur pod and has
# loaded the SLO rules, and KEDA is scaling the consumer. Exits non-zero with
# the first check that fails. Used by CI (.github/workflows/ci.yml).
set -eu

ctx=${KUBE_CONTEXT:-kind-murmur}
k() { kubectl --context "$ctx" "$@"; }
ok() { printf 'ok    %s\n' "$*"; }
fail() { printf 'FAIL  %s\n' "$*" >&2; exit 1; }

# retry SECONDS COMMAND...: run COMMAND every 5 s until it succeeds.
retry() {
	limit=$1; shift
	end=$(($(date +%s) + limit))
	until "$@" >/dev/null 2>&1; do
		[ "$(date +%s)" -lt "$end" ] || return 1
		sleep 5
	done
}

# NATS: three servers, each routed to the other two, and the stream on all.
nats_json() { k -n murmur exec nats-0 -c nats -- wget -qO- "localhost:8222/$1"; }
routes=$(nats_json routez | python3 -c 'import json, sys; print(len(json.load(sys.stdin)["routes"]))')
[ "$routes" -ge 2 ] || fail "nats-0 has $routes routes, want 2"
ok "NATS cluster: nats-0 routes to the other servers"

stream_copies() {
	nats_json 'jsz?streams=true' | python3 -c '
import json, sys
d = json.load(sys.stdin)
for acc in d.get("account_details", []):
    for s in acc.get("stream_detail", []):
        if s["name"] == "TELEMETRY":
            print(1 + len(s["cluster"].get("replicas", [])))
            sys.exit()
print(0)'
}
three_copies() { [ "$(stream_copies)" = 3 ]; }
retry 60 three_copies || fail "TELEMETRY stream has $(stream_copies) copies, want 3"
ok "TELEMETRY stream: 3 copies"

# Data: rows from the 10 fake drones in the last 30 seconds.
recent_drones() {
	k -n murmur exec timescaledb-0 -- psql -U murmur -d murmur -Atc \
		"SELECT count(DISTINCT drone_id) FROM telemetry WHERE received_at > now() - interval '30 seconds'"
}
ten_drones() { [ "$(recent_drones)" -ge 10 ]; }
retry 120 ten_drones || fail "telemetry from $(recent_drones) drones in the last 30 s, want 10"
ok "telemetry stored from $(recent_drones) drones in the last 30 s"

# The host ports from deploy/k8s/kind.yaml.
health=$(curl -fsS http://localhost:8090/api/health) || fail "the map (localhost:8090) does not answer"
echo "$health" | grep -q '"status":"ok"' || fail "gateway health: $health"
ok "map on localhost:8090: $health"
retry 60 curl -fsS http://localhost:3010/api/health || fail "Grafana (localhost:3010) does not answer"
ok "Grafana on localhost:3010"

# Prometheus, through a port-forward for the length of the checks.
k -n monitoring port-forward svc/monitoring-kube-prometheus-prometheus 19090:9090 >/dev/null 2>&1 &
pf=$!
trap 'kill $pf 2>/dev/null || true' EXIT
retry 30 curl -fsS http://localhost:19090/-/ready || fail "Prometheus is not ready"
promql() {
	curl -fsS http://localhost:19090/api/v1/query --data-urlencode "query=$1" |
		python3 -c 'import json, sys; r = json.load(sys.stdin)["data"]["result"]; print(r[0]["value"][1] if r else 0)'
}
# ingester, 2 consumers, 2 gateways, commander, alerter
seven_up() { [ "$(promql 'sum(up{namespace="murmur"})')" = 7 ]; }
retry 120 seven_up || fail "Prometheus scrapes $(promql 'sum(up{namespace="murmur"})') Murmur pods, want 7"
ok "Prometheus scrapes all 7 Murmur pods"

rules_ok() {
	curl -fsS http://localhost:19090/api/v1/rules | python3 -c '
import json, sys
groups = [g for g in json.load(sys.stdin)["data"]["groups"] if g["name"].startswith("murmur")]
rules = [r for g in groups for r in g["rules"]]
sys.exit(0 if len(rules) == 20 and all(r["health"] == "ok" for r in rules) else 1)'
}
retry 120 rules_ok || fail "Prometheus has not loaded Murmur's 20 rules, or one is unhealthy"
ok "Prometheus: Murmur's 20 SLO and health rules loaded and evaluating"

# KEDA: the ScaledObject reads the backlog and drives an HPA.
keda_ready() {
	[ "$(k -n murmur get scaledobject consumer -o jsonpath='{.status.conditions[?(@.type=="Ready")].status}')" = True ] &&
		k -n murmur get hpa keda-hpa-consumer -o jsonpath='{.status.currentMetrics[0].external.current.averageValue}' | grep -q .
}
retry 120 keda_ready || fail "KEDA's ScaledObject for the consumer is not ready, or its HPA has no backlog reading"
ok "KEDA scales the consumer: $(k -n murmur get hpa keda-hpa-consumer -o jsonpath='{.status.currentReplicas}') copies, backlog $(k -n murmur get hpa keda-hpa-consumer -o jsonpath='{.status.currentMetrics[0].external.current.averageValue}') per copy"

echo "all checks passed"
