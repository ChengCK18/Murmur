# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `cmd/ingester`: receives MAVLink over UDP (`-listen`, default `:14560`), gives each drone a drone ID that survives a SITL restart, counts lost frames and heartbeat timeouts, and prints every drone's mode, position, battery and link state once a second.
- The ingester publishes drone telemetry to the NATS JetStream stream `TELEMETRY` as JSON (`-nats`, default `nats://127.0.0.1:4222`; `-nats ""` for console only).
- `deploy/docker-compose.yml` with NATS (JetStream), TimescaleDB and Redis.
- `cmd/consumer`: writes telemetry from JetStream to TimescaleDB (applying the schema migrations on start) and each drone's latest state to Redis (`drone:<tenant>:<drone>`), with nothing lost or duplicated across a restart.
- SITL stream-rate parameters (`deploy/sitl/murmur-streams.parm`) so headless SITL sends position, attitude, system and battery status without a ground station.
- `deploy/sitl/run-fleet.sh` to start several SITL drones sending straight to the ingester.
- `deploy/sitl/run-manual.sh` to start one SITL drone with MAVProxy and QGroundControl for flying by hand, optionally recording to `testdata/`.
- `deploy/sitl/record.py` to fly scripted idle, mission and restart sessions and record them as `.tlog` files.
- Test recordings `testdata/idle.tlog`, `testdata/flight.tlog` and `testdata/restart.tlog`, and the `testdata/missions/square.txt` mission.
