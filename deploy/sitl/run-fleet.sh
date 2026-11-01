#!/usr/bin/env bash
# Start a fleet of headless ArduCopter SITL instances that send MAVLink straight to the ingester (mode C).
# Usage: deploy/sitl/run-fleet.sh [count] [ingester host:port]
# Ctrl-C stops every instance. Each instance runs in .sitl/<n>/ and logs to .sitl/<n>/sitl.log.
set -euo pipefail
COUNT=${1:-3}
TARGET=${2:-127.0.0.1:14560}
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
AP=${ARDUPILOT:-$HOME/ardupilot}
BIN="$AP/build/sitl/bin/arducopter"
DEFAULTS="$AP/Tools/autotest/default_params/copter.parm,$ROOT/deploy/sitl/murmur-streams.parm"
BASE_LAT=-35.363262 BASE_LNG=149.165237 BASE_ALT=584

[[ -x "$BIN" ]] || { echo "arducopter not found at $BIN (set ARDUPILOT or build SITL)" >&2; exit 1; }

trap 'trap - INT TERM EXIT; kill $(jobs -p) 2>/dev/null; wait' INT TERM EXIT

for i in $(seq 0 $((COUNT - 1))); do
  dir="$ROOT/.sitl/$i"
  mkdir -p "$dir"
  lat=$(echo "$BASE_LAT + $i * 0.0003" | bc -l)   # about 33 m apart
  (cd "$dir" && exec "$BIN" --model + -I "$i" --sysid $((i + 1)) \
     --home "$lat,$BASE_LNG,$BASE_ALT,0" \
     --defaults "$DEFAULTS" \
     --serial0 "udpclient:$TARGET" >"$dir/sitl.log" 2>&1) &
  echo "instance $i: sysid $((i + 1)), home $lat,$BASE_LNG, log $dir/sitl.log"
done
echo "Started $COUNT SITL instances sending to $TARGET; Ctrl-C to stop them"
wait
