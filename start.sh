#!/usr/bin/env bash
# Starts Murmur and waits until the drones are flying.
#
#   ./start.sh            build and start everything, with 10 simulated drones
#   ./start.sh --sitl     the same, plus 3 ArduPilot SITL drones (first build: about 5 minutes)
#   ./start.sh --open     also open the map in the browser
#   ./start.sh status     what is running, and how many drones are live
#   ./start.sh logs [service]   follow the logs, of one service or all
#   ./start.sh stop       stop everything; the stored data is kept
#
# Needs only Docker with Compose v2, and bash 3.2 or later (macOS's own).
set -euo pipefail

cd "$(dirname "$0")"
COMPOSE=(docker compose -f deploy/docker-compose.yml --profile sitl)
MAP=http://localhost:8080
GRAFANA=http://localhost:3000
PROMETHEUS=http://localhost:9090

# Colour only on a terminal, so piped output stays plain.
if [ -t 1 ]; then B=$'\033[1m' R=$'\033[31m' N=$'\033[0m'; else B= R= N=; fi
bold() { printf '%s%s%s\n' "$B" "$*" "$N"; }
fail() {
	printf '%s%s%s\n' "$R" "$*" "$N" >&2
	exit 1
}

# live prints how many drones the gateway has heard a heartbeat from lately.
live() {
	local body
	body=$(curl -fsS --max-time 2 "$MAP/api/drones" 2>/dev/null) || {
		echo 0
		return 0
	}
	grep -o '"link":"ok"' <<<"$body" | wc -l || true
}

summary() {
	echo
	bold "Murmur is running"
	printf '  %-11s %-24s %s\n' \
		"Map" "$MAP" "log in as demo / murmur to send commands" \
		"Grafana" "$GRAFANA" "dashboards; admin / murmur to edit" \
		"Prometheus" "$PROMETHEUS" ""
	printf '  %-11s %s\n' "Drones" "$(live) live"
	echo
	echo "  ./start.sh status | logs [service] | stop"
}

needs_docker() {
	command -v docker >/dev/null || fail "Docker is not installed: https://docs.docker.com/get-docker/"
	docker compose version >/dev/null 2>&1 || fail "Docker Compose v2 is missing (the 'docker compose' command)."
	docker info >/dev/null 2>&1 || fail "Docker is not running, or this user cannot reach it."
}

start() {
	local sitl=false open=false
	for arg in "$@"; do
		case $arg in
		--sitl) sitl=true ;;
		--open) open=true ;;
		*) fail "unknown option: $arg (try --sitl or --open)" ;;
		esac
	done
	needs_docker

	local services=() want=10
	if $sitl; then
		want=13
		bold "Building and starting Murmur with SITL (the first build compiles ArduPilot, about 5 minutes)"
	else
		# Every service but SITL, so a SITL started earlier is left alone.
		# A read loop, not mapfile: macOS ships bash 3.2, which lacks it.
		while IFS= read -r svc; do services+=("$svc"); done < <(docker compose -f deploy/docker-compose.yml config --services)
		bold "Building and starting Murmur"
	fi
	# ${a[@]+...}: bash before 4.4 calls an empty array unbound under set -u.
	if ! "${COMPOSE[@]}" up -d --build --wait ${services[@]+"${services[@]}"}; then
		echo
		"${COMPOSE[@]}" ps
		fail "Something did not start. See its logs with: ./start.sh logs <service>"
	fi

	printf 'Waiting for the drones'
	local n=0
	for _ in $(seq 60); do
		n=$(live)
		[ "$n" -ge "$want" ] && break
		printf '.'
		sleep 1
	done
	echo
	if [ "$n" -lt "$want" ]; then
		echo "Only $n of $want drones are live so far. SITL drones can take a minute after the build."
	fi

	summary
	if $open; then
		xdg-open "$MAP" >/dev/null 2>&1 || open "$MAP" >/dev/null 2>&1 || echo "Open $MAP in your browser."
	fi
}

case ${1:-} in
stop)
	needs_docker
	"${COMPOSE[@]}" stop
	echo "Stopped. The data is kept; ./start.sh starts it again."
	;;
status)
	needs_docker
	"${COMPOSE[@]}" ps --format 'table {{.Service}}\t{{.Status}}'
	echo
	echo "Drones live: $(live)"
	;;
logs)
	needs_docker
	shift
	"${COMPOSE[@]}" logs -f --tail 50 "$@"
	;;
-h | --help | help)
	sed -n '2,10p' "$0" | sed 's/^# \{0,1\}//'
	;;
*)
	start "$@"
	;;
esac
