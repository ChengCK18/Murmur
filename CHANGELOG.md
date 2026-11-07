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
- `loadtest/failures/` (`make failures`): kills the consumer, the database, the ingester and the gateway, and drops packets, under load, and prints pass or fail against the plan's limits; results are in `docs/results.md`.
- The ingester keeps drone IDs in the `drones` table (`-db`), so they survive an ingester restart.
- `cmd/consumer`: writes telemetry from JetStream to TimescaleDB (applying the schema migrations on start) and each drone's latest state to Redis (`drone:<tenant>:<drone>`), with nothing lost or duplicated across a restart.
- The ingester relays commands to drones from the socket it reads on, as a ground station (system 255, component 190), refusing any that have expired or whose drone it has not heard since it started. `-commands=false` makes it telemetry only.
- `cmd/commander`: the command service, on `:9104` for metrics. It records every command in the `commands` table before sending it, resends up to 3 times with MAVLink's `confirmation` field, allows one command in flight per drone per type, and gives each a final status within its 5 second life. Commands left in flight by a restart are marked `unknown`.
- The commander checks each command against the drone's latest state in Redis (`-redis`) and refuses it with a clear reason, such as "Takeoff needs the drone armed and in GUIDED mode". The drone still has the final say.
- Demo login on the gateway (`POST /api/login`, `POST /api/logout`, `GET /api/session`; `-user`, `-password`): a session cookie that lasts 8 hours and survives a gateway restart. Logged-in browsers send commands over the WebSocket (`{"type":"command",...}`) and get `command_status` messages; `GET /api/drones/{id}/commands` reloads a drone's latest 20. `-commands=false` turns the command path off.
- `fakegen` drones answer commands and act them out, so commands work on the whole fleet: arm and disarm, mode changes, takeoff, return to launch, land, and mission upload and flight. `-command-drop` and `-command-reject` make them lose or reject a share of commands. They now report AUTO while on their routes.
- A command panel on the map for the selected drone: log in, arm, disarm, set mode, take off, return to launch and land, with a confirmation for arm and takeoff. Every command shows its status and reason, and the list comes back after a reconnect.
- The commander takes one command for many drones (up to 1,000), spreads the sends over about 100 ms and tracks each drone on its own, with the `fleet_id` in the `commands` table.
- `{"type":"fleet_command",...}` on the WebSocket, answered by `fleet_started`, and `GET /api/fleets/{id}/commands` to reload one.
- "Return all to launch" and "Land all" above the drone list, with a confirmation, totals such as "18 accepted, 1 rejected, 1 timed out", and the failures listed drone by drone.
- Mission upload: the commander runs MAVLink's mission handshake through the ingester, with a timeout and 3 tries at each step, answering repeated requests, and keeps missions in a `missions` table. One upload runs per drone, and other commands to that drone are refused until it ends. Start mission needs one uploaded.
- `{"type":"mission_upload",...}` on the WebSocket and `GET /api/drones/{id}/mission` on the gateway.
- Mission planning on the map: click to add waypoints, upload them to the selected drone, start the mission, and see the route it holds.
- `cmd/alerter` (metrics on `:9105`): its own durable consumer on `TELEMETRY` raises and clears low battery (`-low-battery`, default 20%), link lost and geofence breach alerts, writes them to a new `events` table and publishes them on `alert.<drone_id>`. The geofence is kept in a `geofences` table and checked by the platform, so it works for every drone.
- The gateway sends every browser `{"type":"alert",...}` and `{"type":"geofence",...}`, and serves `GET /api/alerts`, `GET /api/geofence` and, logged in, `PUT /api/geofence`.
- Alerts on the map: a list of the alerts raised now above the drone list, drones with an alert outlined in red, and a geofence drawn and removed on the map.
- Flight replay: `GET /api/drones/{id}/history?from=&to=` on the gateway (`-db`) reads a drone's stored flight, thinned to about 3,000 points, and the drone's panel plays it back with a time scrubber at 1, 10 or 60 times speed.
- Grafana on `http://localhost:3000`, provisioned from `deploy/grafana/`: a *Murmur pipeline* dashboard (rates, loss, queue lag, write time, both latency spans, commands) and a *Murmur fleet* dashboard (messages stored, alerts and the command log from TimescaleDB). Viewing needs no login.
- SITL stream-rate parameters (`deploy/sitl/murmur-streams.parm`) so headless SITL sends position, attitude, system and battery status without a ground station.
- `deploy/sitl/run-fleet.sh` to start several SITL drones sending straight to the ingester (by name or IP address), lined up south of `fakegen`'s grid so the two fleets can run together.
- `docker compose --profile sitl up` (`make sitl`) adds 3 ArduCopter SITL drones to the stack. The first build compiles ArduPilot, about 5 minutes; plain `docker compose up` does not need it.
- `deploy/sitl/run-manual.sh` to start one SITL drone with MAVProxy and QGroundControl for flying by hand, optionally recording to `testdata/`.
- `deploy/sitl/record.py` to fly scripted idle, mission and restart sessions and record them as `.tlog` files.
- Test recordings `testdata/idle.tlog`, `testdata/flight.tlog` and `testdata/restart.tlog`, and the `testdata/missions/square.txt` mission.

### Changed
- The consumer writes each batch in one statement, raising its limit from about 5,700 to about 14,000 rows a second.

### Fixed
- A cleared link-lost alert now says "Heartbeat back" instead of nothing.
- `fakegen` follows the ingester to a new IP address, which it can get when restarted; before, the drones kept sending to the old one until `fakegen` was restarted too.
- The whole stack comes back after a reboot: NATS, TimescaleDB, Redis and Prometheus now have a restart policy, where before the services that need them restarted forever.
- `murmur_ingester_frames_lost_total`, `murmur_ingester_link_losses_total` and the other link counters no longer fall when a drone is rebound to a new address.
- The ingester no longer loses packets above 250 drones: it reads the socket in its own goroutine, with a 4 MB buffer (`-read-buffer`) and a queue (`-queue`) whose drops are counted in `murmur_ingester_queue_dropped_total`. Healthy drones no longer flip to "link lost" under load.
- A drone with no GPS fix yet is no longer drawn at latitude 0, longitude 0, which pulled the map out to show half the world when the page opened during a SITL drone's first seconds.
- The map was blank: MapLibre could not find its tile worker in the built app, so no tile was ever loaded. The worker is now bundled and the map draws streets, water and land cover.
- The gateway answers a missing script or stylesheet with 404 instead of the app's page.
- The ingester no longer crashes on its first report when the `drones` table holds a drone that has not sent anything since the ingester started.
