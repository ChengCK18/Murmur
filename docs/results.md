# Load test results

Murmur met its ingest target: **10,000 messages a second from 1,000 drones, held for 5 minutes, with no packets lost.** Half of those messages reached the database within 184 ms and 99% within about 500 ms.

Getting there took two fixes. The first run lost 10% of its packets at the UDP socket and stored only 5,664 rows a second.

## How it was measured

- **Machine:** one desktop running everything, including the load generator: Intel Core i5-8400 (6 cores), 16 GB of memory.
- **Stack:** the Compose stack from `make up`, with no tuning beyond the fixes below.
- **Load:** `cmd/fakegen`, sending real MAVLink over UDP from one source port per drone. Each drone sends 10 messages a second, on circle, line and survey routes.
- **Browsers:** `loadtest/wsprobe` stands in for two: one following the whole map by area, one following 100 drones by ID.
- **Each level:** 25 s to settle, then 60 s measured, then wait for the consumer to catch up. The soak measured 5 minutes instead of 60 s.
- **To repeat it:** `make up`, then `loadtest/run.py <label>`. Results are in `loadtest/results/`, and `loadtest/plot.py` draws these charts.

Latency figures come from Prometheus histograms whose buckets double in size, so they are estimates. The p99 of about 500 ms means "between 256 and 512 ms", and any latency over 16 s shows as 16,384 ms, the top of the histogram.

## Results

### Throughput

![Rows written per second by fleet size, before and after the fixes](img/throughput.svg)

Before the fixes, writes fell behind between 500 and 1,000 drones. After them, writes keep up with the drones to 10,000 a second and level off at 13,000 to 14,000.

| Drones | Sent/s | Read by ingester/s, before | after | Rows written/s, before | after |
|---:|---:|---:|---:|---:|---:|
| 100 | 1,000 | 1,000 | 999 | 997 | 1,001 |
| 250 | 2,500 | 2,417 | 2,507 | 2,421 | 2,500 |
| 500 | 5,000 | 4,782 | 5,000 | 4,781 | 5,001 |
| **1,000** | **10,000** | 9,004 | **10,000** | 5,664 | **10,000** |

Small differences from the sent rate, such as 2,507 at 250 drones, come from where the 60 s window falls.

### Packet loss

![Share of packets lost by fleet size, before and after the fixes](img/loss.svg)

| Drones | Lost, before | Lost, after | False "link lost" events, before | after |
|---:|---:|---:|---:|---:|
| 100 | 0% | 0.1% | 0 | 0 |
| 250 | 3.3% | 0% | 0 | 0 |
| 500 | 4.4% | 0% | 0 | 0 |
| 1,000 | 10.0% | 0% | 67 | 0 |

The 0.1% at 100 drones is 999 packets read against 1,000 sent, and the frame counter recorded no lost frames in that run.

### Latency

![Receive to database commit at p99 by fleet size, before and after the fixes](img/latency.svg)

Times are in milliseconds. "Commit" is from the ingester receiving a packet to the database commit. "Send" is from the ingester receiving a packet to the gateway writing it to a browser.

| Drones | Commit p50, before | after | Commit p99, before | after | Send p50, after | Send p99, after |
|---:|---:|---:|---:|---:|---:|---:|
| 100 | 171 | 111 | 493 | 253 | 100 | 252 |
| 250 | 187 | 137 | 499 | 482 | 99 | 253 |
| 500 | 207 | 144 | 504 | 439 | 104 | 253 |
| 1,000 | over 16,000 | 194 | over 16,000 | 503 | 124 | 254 |

Send times here are for drones followed by ID. Their p99 is set by the 250 ms throttle, not by load. Drones followed by map area get one update a second, and at 1,000 drones their send p99 was 501 ms in this run and 284 ms in the soak.

### Soak: 1,000 drones for 5 minutes

| Measure | Result |
|---|---:|
| Read by the ingester | 10,000/s |
| Rows written | 10,000/s |
| Packets lost | 0 |
| False "link lost" events | 0 |
| Commit p50 / p99 | 184 ms / 502 ms |
| Send p50 / p99 | 129 ms / 254 ms |
| Total CPU, all services | about 2.5 of 6 cores |

### Past the target

| Drones | Sent/s | Packets lost | Rows written/s | Queue at end of run | Commit p99 | Send p99 |
|---:|---:|---:|---:|---:|---:|---:|
| 1,500 | 15,000 | 0% | 14,064 | 110,658 | 8,150 ms | 251 ms |
| 2,000 | 20,000 | 0% | 13,291 | 682,081 | over 16,000 ms | 253 ms |
| 3,000 | 30,000 | 0% | 12,582 | 1,783,842 | over 16,000 ms | 494 ms |

The ingester, NATS and the gateway carried 30,000 messages a second with no loss, while NATS was close to one full core. The consumer is the limit, at 13,000 to 14,000 rows a second. Above that, messages wait in JetStream and the database falls behind, but the map stays live because the gateway reads the stream directly. The queue cleared within 130 s of the load stopping, and no message was lost. A backlog older than JetStream's 10-minute retention would lose messages, so a rate above the consumer's limit is safe only for short bursts.

## The two bottlenecks

### 1. The ingester lost packets at its socket

Loss started at 250 drones, while the ingester used only about 30% of a core. The kernel's counter for the socket showed why: 11,355 packets dropped in 30 s at 1,000 drones, with the receive queue at 157 KB of its 212 KB limit.

At 10,000 messages a second that buffer holds about 30 ms of traffic, and the same loop that read the socket also decoded, registered and published each packet. Any short pause overflowed it. Lost heartbeats then made healthy drones flip to "link lost", which is why the map showed about 13% of the fleet as lost during an earlier run.

**Fix:** a 4 MB socket buffer, and a separate goroutine that only reads the socket and queues packets for processing. `received_at` is still stamped at the read, so time in the queue counts as latency. If the queue ever fills, the drops now appear in `murmur_ingester_queue_dropped_total`.

The kernel caps the buffer at `net.core.rmem_max`. This machine allows 4 MB. On a machine with a lower limit the buffer will be smaller, and the queue does more of the work.

### 2. The consumer wrote one statement per row

The consumer stored a batch as up to 500 separate `INSERT` statements, which held it to about 5,700 rows a second with TimescaleDB at 78% CPU. A backlog built up until p50 latency passed the histogram's 16 s limit.

**Fix:** one `INSERT` per batch, with the columns sent as arrays through `unnest`. The same rows now take TimescaleDB about 47% CPU at nearly twice the rate. `ON CONFLICT DO NOTHING` still drops redelivered rows: the Phase 2 test, which kills the consumer partway through 20,000 messages, passed unchanged with exactly 20,000 rows.

## Limits of these results

- **One machine.** The load generator shares the CPU with the platform, and all traffic stays on this machine, so these numbers leave out real network delay and loss.
- **Fake drones are steady.** They send evenly spaced messages. Real fleets send bursts, which would test the buffers harder.
- **Not yet measured under the fixed stack:** the map page itself. With a real browser and 1,000 drones it drew 4.4 frames a second, but that was before the fixes and on software rendering with no GPU, so it says little about a normal machine.
- **Partitioned consumers were not added.** The guide suggests them if the consumer is the limit. The target is met with 30% or more to spare, so they can wait until a higher target calls for them.
