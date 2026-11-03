# Changelog

All notable changes to this project are documented in this file.

The format is based on [Keep a Changelog](https://keepachangelog.com/en/1.1.0/),
and this project adheres to [Semantic Versioning](https://semver.org/spec/v2.0.0.html).

## [Unreleased]

### Added
- `cmd/ingester`: receives MAVLink over UDP (`-listen`, default `:14560`), gives each drone a drone ID that survives a SITL restart, counts lost frames and heartbeat timeouts, and prints every drone's mode, position, battery and link state once a second.
- The ingester publishes drone telemetry to the NATS JetStream stream `TELEMETRY` as JSON (`-nats`, default `nats://127.0.0.1:4222`; `-nats ""` for console only).
- Prometheus metrics on `/metrics`: the ingester on `:9101` (packets, decode errors, lost frames, link losses, published and publish errors) and the consumer on `:9102` (inserted, duplicates, lag, batch size and write time, receive-to-commit latency); `-metrics ""` turns them off.
- The consumer's `-redis` also takes a `redis://host:port/N` URL to choose a database.
- `cmd/fakegen`: a fleet of fake drones (`-count`, `-target`, `-duration`, `-messages`), each with its own UDP port and sequence counter, flying circles on a grid with the SITL message rates.
- `deploy/docker-compose.yml` with NATS (JetStream), TimescaleDB, Redis, the ingester, the consumer, 10 fake drones and Prometheus (`:9090`); `deploy/Dockerfile` builds any command's image.
- `cmd/gateway` on `:8080`: a WebSocket (`/ws`) where browsers subscribe to up to 100 drones, get a snapshot and then each drone's latest state at most every 250 ms, with link-lost changes pushed; `GET /api/drones` and `GET /api/health`; metrics on `:9103`.
- The ingester keeps drone IDs in the `drones` table (`-db`), so they survive an ingester restart.
- `cmd/consumer`: writes telemetry from JetStream to TimescaleDB (applying the schema migrations on start) and each drone's latest state to Redis (`drone:<tenant>:<drone>`), with nothing lost or duplicated across a restart.
- SITL stream-rate parameters (`deploy/sitl/murmur-streams.parm`) so headless SITL sends position, attitude, system and battery status without a ground station.
- `deploy/sitl/run-fleet.sh` to start several SITL drones sending straight to the ingester.
- `deploy/sitl/run-manual.sh` to start one SITL drone with MAVProxy and QGroundControl for flying by hand, optionally recording to `testdata/`.
- `deploy/sitl/record.py` to fly scripted idle, mission and restart sessions and record them as `.tlog` files.
- Test recordings `testdata/idle.tlog`, `testdata/flight.tlog` and `testdata/restart.tlog`, and the `testdata/missions/square.txt` mission.

### Fixed
- The ingester no longer crashes on its first report when the `drones` table holds a drone that has not sent anything since the ingester started.
