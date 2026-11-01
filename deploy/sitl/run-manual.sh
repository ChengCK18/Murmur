#!/usr/bin/env bash
# Start one headless ArduCopter SITL with MAVProxy (mode B) plus QGroundControl, for flying by hand.
# Usage: deploy/sitl/run-manual.sh [recording-name] [extra sim_vehicle.py args]
# With a name, MAVProxy records to testdata/<name>.tlog (and will not overwrite an existing one);
# without, it records to $ARDUPILOT/ArduCopter/mav.tlog, which every start overwrites.
# MAVProxy runs in this terminal and sends to QGroundControl on UDP 14550 (its default) and to the
# Murmur ingester on UDP 14560. Exiting MAVProxy (Ctrl-C or "exit") also closes QGroundControl.
set -euo pipefail
ROOT=$(cd "$(dirname "$0")/../.." && pwd)
AP=${ARDUPILOT:-$HOME/ardupilot}
VENV=${VENV:-$HOME/venv-ardupilot}
QGC=${QGC:-$HOME/Applications/QGroundControl-x86_64.AppImage}
INGESTER=${INGESTER:-127.0.0.1:14560}

[[ -x "$QGC" ]] || { echo "QGroundControl not found at $QGC (set QGC)" >&2; exit 1; }
[[ -f "$VENV/bin/activate" ]] || { echo "virtualenv not found at $VENV (set VENV)" >&2; exit 1; }

LOG_ARGS=()
if [[ $# -gt 0 && $1 != -* ]]; then
  log="$ROOT/testdata/$1.tlog"
  shift
  [[ ! -e "$log" ]] || { echo "$log already exists; move it or pick another name" >&2; exit 1; }
  mkdir -p "$ROOT/testdata"
  LOG_ARGS=(--mavproxy-args="--logfile=$log")
  echo "Recording to $log"
fi

# QGroundControl in its own process group, so the whole AppImage process tree can be stopped at exit.
setsid "$QGC" >/tmp/qgroundcontrol.log 2>&1 &
QGC_PID=$!
trap 'kill -TERM -- -$QGC_PID 2>/dev/null || true' EXIT

set +u; source "$VENV/bin/activate"; set -u
cd "$AP/ArduCopter"
sim_vehicle.py -v ArduCopter -f quad --console --map --out "udp:$INGESTER" "${LOG_ARGS[@]}" "$@"
