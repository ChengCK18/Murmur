#!/bin/sh
# Unit tests for Murmur's alert rules (make k8s-rules-test): renders the
# chart's PrometheusRule, turns it into a plain Prometheus rules file and runs
# deploy/k8s/slo-rules.test.yaml against it with promtool, from the same
# Prometheus image Docker Compose uses. Needs Helm, Python 3 with PyYAML, and
# Docker; no cluster.
set -eu
cd "$(dirname "$0")"
out=$(mktemp -d)
trap 'rm -rf "$out"' EXIT
helm template murmur murmur --namespace murmur \
	--api-versions monitoring.coreos.com/v1/PrometheusRule -s templates/slo-rules.yaml |
	python3 -c 'import sys, yaml; yaml.safe_dump({"groups": yaml.safe_load(sys.stdin)["spec"]["groups"]}, sys.stdout, sort_keys=False)' \
	>"$out/slo-rules.yaml"
cp slo-rules.test.yaml "$out/"
docker run --rm --user "$(id -u)" -v "$out:/rules:ro" --entrypoint promtool prom/prometheus:v3.15.1 \
	test rules /rules/slo-rules.test.yaml
