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
- `cmd/fakegen`: a fleet of fake drones (`-count`, `-target`, `-duration`, `-messages`, and `-drop` to lose a share of packets on purpose), each with its own UDP port and sequence counter, flying circles, back-and-forth lines or lawnmower survey patterns on a grid and sending 10 messages a second (1,000 drones make the 10,000 messages a second load-test target).
- `deploy/docker-compose.yml` with NATS (JetStream), TimescaleDB, Redis, the ingester, the consumer, the gateway serving the map on `http://localhost:8080`, 10 fake drones and Prometheus (`:9090`); `deploy/Dockerfile` builds any command's image.
- `web/`: the live map (React, MapLibre, OpenFreeMap tiles): one marker per drone turned to its heading and labelled with altitude and battery, greyed out when its link is lost; a drone list that centres the map on a click; a banner and automatic reconnect (1, 2, 4, 8, then every 10 s) when the gateway goes away.
- The map hides drones not heard from for 10 minutes (gateway `-stale`; `0` shows every drone ever seen), so drones from earlier runs no longer pile up greyed out. A drone that flies again comes back.
- `cmd/gateway` on `:8080`: a WebSocket (`/ws`) where browsers subscribe to up to 100 drones, get a snapshot and then each drone's latest state at most every 250 ms, with link-lost changes pushed; `GET /api/drones` and `GET /api/health`; metrics on `:9103`, including receive-to-send latency.
- Map-area subscriptions: `{"type":"subscribe_area","bbox":[minLng,minLat,maxLng,maxLat]}` on the WebSocket follows every drone inside an area, once a second each and outside the 100-drone limit. The map follows what is on screen, plus a margin, instead of the first 100 drones, the selected drone stays live wherever the map is, and the list shows the whole fleet, refreshed every 10 seconds.
- `loadtest/`: runs `fakegen` at rising fleet sizes against the Compose stack and saves throughput, loss and both latency spans; results and charts are in `docs/results.md`.
- The ingester keeps drone IDs in the `drones` table (`-db`), so they survive an ingester restart.
- `cmd/consumer`: writes telemetry from JetStream to TimescaleDB (applying the schema migrations on start) and each drone's latest state to Redis (`drone:<tenant>:<drone>`), with nothing lost or duplicated across a restart.
- SITL stream-rate parameters (`deploy/sitl/murmur-streams.parm`) so headless SITL sends position, attitude, system and battery status without a ground station.
- `deploy/sitl/run-fleet.sh` to start several SITL drones sending straight to the ingester (by name or IP address), lined up south of `fakegen`'s grid so the two fleets can run together.
- `docker compose --profile sitl up` (`make sitl`) adds 3 ArduCopter SITL drones to the stack. The first build compiles ArduPilot, about 5 minutes; plain `docker compose up` does not need it.
- `deploy/sitl/run-manual.sh` to start one SITL drone with MAVProxy and QGroundControl for flying by hand, optionally recording to `testdata/`.
- `deploy/sitl/record.py` to fly scripted idle, mission and restart sessions and record them as `.tlog` files.
- Test recordings `testdata/idle.tlog`, `testdata/flight.tlog` and `testdata/restart.tlog`, and the `testdata/missions/square.txt` mission.

### Changed
- The consumer writes each batch in one statement, raising its limit from about 5,700 to about 14,000 rows a second.

### Fixed
- `fakegen` follows the ingester to a new IP address, which it can get when restarted; before, the drones kept sending to the old one until `fakegen` was restarted too.
- The whole stack comes back after a reboot: NATS, TimescaleDB, Redis and Prometheus now have a restart policy, where before the services that need them restarted forever.
- `murmur_ingester_frames_lost_total`, `murmur_ingester_link_losses_total` and the other link counters no longer fall when a drone is rebound to a new address.
- The ingester no longer loses packets above 250 drones: it reads the socket in its own goroutine, with a 4 MB buffer (`-read-buffer`) and a queue (`-queue`) whose drops are counted in `murmur_ingester_queue_dropped_total`. Healthy drones no longer flip to "link lost" under load.
- A drone with no GPS fix yet is no longer drawn at latitude 0, longitude 0, which pulled the map out to show half the world when the page opened during a SITL drone's first seconds.
- The map was blank: MapLibre could not find its tile worker in the built app, so no tile was ever loaded. The worker is now bundled and the map draws streets, water and land cover.
- The gateway answers a missing script or stylesheet with 404 instead of the app's page.
- The ingester no longer crashes on its first report when the `drones` table holds a drone that has not sent anything since the ingester started.
