# Two-minute demo

What to show, in order, with what to say. Start with the stack running (`make up`), the map open at http://localhost:8080 and Grafana at http://localhost:3000 in a second tab.

If the demo machine fails, fall back to the screen recording of this script.

| Time | Do | Say |
|---|---|---|
| 0:00 | The map, with the fleet flying. | "Ten simulated drones, each sending real MAVLink over UDP, ten messages a second." |
| 0:10 | Click a drone in the list. | "Its state comes over a WebSocket. The browser only follows what is on screen, so a thousand drones do not flood it." |
| 0:20 | Log in as `demo` / `murmur`. Click **Land**. | "Commands go through a command service that checks them, records them and retries them. Every one ends with a final status: here, accepted." |
| 0:35 | Click **Takeoff** while it is landing. | "Refused with a reason before it is sent: takeoff needs GUIDED mode. The drone still has the final say." |
| 0:45 | **Plan on map**, click three points, **Upload**, then **Start mission**. | "Missions use MAVLink's own upload handshake, where the drone asks for each item." |
| 1:00 | **Return all to launch**, confirm. | "One command to the whole fleet, tracked drone by drone: ten accepted." |
| 1:10 | **Draw geofence**, a small triangle away from the drones, **Save**. | "Alerts are checked by a separate service. Every drone is now outside the fence." Then **Remove geofence**. |
| 1:25 | In the drone's panel, **Load** under Replay, then **Play**. | "Every message is stored in TimescaleDB, so any flight can be replayed." |
| 1:40 | The Grafana tab, *Murmur pipeline*. | "The pipeline watches itself: message rates, queue lag and latency." |
| 1:50 | [docs/results.md](results.md). | "Load tested at 10,000 messages a second from 1,000 drones with no loss, and every service killed under load without losing data." |

## If something goes wrong

- **No drones on the map:** `docker compose -f deploy/docker-compose.yml ps`. Every service should be running; `make up` starts any that are not.
- **Greyed-out drones beside the live ones:** drones from an earlier run, hidden after 10 minutes.
- **A command times out:** the drone did not answer within 3 tries. It shows as timed out, never as pending.
