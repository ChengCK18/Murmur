# Murmur on Kubernetes

The whole platform runs on a local [kind](https://kind.sigs.k8s.io/) cluster from one Helm chart, with the parts that can share work spread over three nodes, the consumer autoscaled on its backlog, and alerts on two service-level objectives. CI builds the same cluster on every push and checks it.

```sh
make kind-up      # a cluster of one control plane and three workers, with Murmur's images loaded
make k8s-up       # kube-prometheus-stack, KEDA, then Murmur's chart
make k8s-smoke    # checks the running platform end to end
```

| What | Where |
|---|---|
| The map | http://localhost:8090 (`demo` / `murmur`) |
| Grafana | http://localhost:3010 (anonymous view; `admin` / `murmur` to edit) |
| MAVLink in | UDP 127.0.0.1:14570 |

The ports are apart from Docker Compose's, so both stacks can run at once. `make k8s-down` removes the charts and keeps the volumes; `make kind-down` deletes the cluster. After a code change, `make kind-up` rebuilds and reloads the images. You need kind, kubectl, Helm and Docker, and about 3 GB of free memory.

## Where everything runs

```
            murmur-worker (edge)       murmur-worker2           murmur-worker3
            ┌──────────────────┐       ┌──────────────────┐     ┌──────────────────┐
UDP 14570 ─▶│ ingester (pinned)│       │                  │     │                  │
            │ nats-1           │◀─────▶│ nats-0           │◀───▶│ nats-2           │
            │ consumer, gateway│       │ consumer, gateway│     │ ...              │
            └──────────────────┘       └──────────────────┘     └──────────────────┘
                        the rest wherever the scheduler finds room
```

Only two placements are rules: the ingester runs on the node labelled `murmur.io/edge`, where the UDP port is open, and no two NATS servers share a node. The consumers and gateways are spread over the nodes where there is room, and the rest go wherever the scheduler finds it, so a reinstall can move them.

A kind node is a Docker container, so the four "machines" share one computer: the cluster shows how the platform is spread and how it fails over, not how much it carries. The control plane runs Kubernetes itself and none of Murmur.

## What scales, and what cannot

The replica counts follow what each service can share. Most of the chart's choices come from this table.

| Service | Copies | Why |
|---|---|---|
| NATS | 3, one per node | JetStream keeps a copy of the stream on each server and accepts a write once a majority has it. Three is the smallest count that survives losing one: 2 of 3 is still a majority, while 1 of 2 is not, so the chart refuses an even count. One per node is enforced, or a lost node could take two copies. |
| consumer | 2 to 6, by backlog | Every copy pulls from the same durable consumer, so NATS gives each message to one of them, and a redelivered message is dropped by the insert's `ON CONFLICT`. |
| gateway | 2 | Sessions are in Redis and every copy reads the whole stream, so any copy can serve any browser, with no sticky sessions. |
| ingester | 1, on the edge node | It keeps each drone's state: who it is (UDP source address and MAVLink system ID), its sequence numbers for loss counting, its heartbeat timer and where to send its commands. UDP has no connections to share out, so two copies would each see half of every drone. It scales by site, one ingester per group of drones, not by copies. |
| commander | 1 | It takes command requests on plain NATS subscriptions, not a queue group, so two copies would each send every command. |
| alerter | 1 | It keeps each drone's alert state in memory and answers on plain subscriptions, like the commander. |
| TimescaleDB, Redis | 1 | Single servers, as in Compose. The database is the platform's long-term ceiling. |

The ingester, commander and alerter use `strategy: Recreate`: a rolling update starts the new copy before stopping the old one, and for those seconds both would act on every message. Commands sent in that gap fail at once rather than run later, since they are never queued.

The ingester's Service is a NodePort with `externalTrafficPolicy: Local`. By default a node port may forward to a pod on another node and rewrite the source address on the way, and the ingester tells drones apart by that address and replies to it. `Local` delivers only to the pod on the node the packet reached, with the address intact, which works because the ingester is pinned to that node.

## Autoscaling the consumer

[KEDA](https://keda.sh/) reads the backlog of the `db-writer` consumer (messages waiting plus delivered but unacknowledged) from NATS's monitoring port, and drives a HorizontalPodAutoscaler from it: about one copy per 1,000 waiting messages, between 2 and 6. It scales up at once and down after a minute of low backlog. To find the server leading `db-writer`, KEDA asks any server for the addresses the cluster advertises, its pod IPs, and then asks the leader.

It scales on backlog rather than CPU because the consumer is I/O-bound. It spends its time waiting on database commits, so its CPU stays low while it falls behind, and a CPU-based autoscaler would not notice. With KEDA in the cluster the Deployment leaves out `replicas`, or every `helm upgrade` would reset the count the autoscaler chose. Without KEDA, or with `consumer.autoscaling.enabled: false`, two fixed copies run.

**Measured on kind**, one machine running the cluster and the load generator:

- **500 extra fake drones for 3 minutes** (5,100 messages a second): 899,998 messages sent, none dropped, p99 receive-to-commit under 1 s. The backlog peaked at about 360 messages per copy, under the threshold, so the two minimum copies absorbed it and KEDA rightly added none.
- **The same load with the threshold lowered to 50**, to show the mechanism: 2 to 6 copies within 10 s of the load arriving, then 6, 5, 4, 2 as the backlog stayed low, back to 2 a minute after the load ended.

More copies help until the database is the limit. In the [load tests](results.md), one consumer wrote 13,000 to 14,000 rows a second with TimescaleDB under half busy.

## Monitoring

[kube-prometheus-stack](https://github.com/prometheus-community/helm-charts/tree/main/charts/kube-prometheus-stack) runs Prometheus, Grafana, Alertmanager, node-exporter and kube-state-metrics in the `monitoring` namespace. Murmur's chart brings one ServiceMonitor, which matches every Murmur Service with a port named `metrics` and scrapes each pod behind it every 5 s, so the targets follow the replica counts without a list. Grafana loads the two dashboards from `deploy/grafana/dashboards` unchanged, next to the cluster's own.

kind's etcd, scheduler, controller manager and kube-proxy listen on localhost only, so they are not scraped; otherwise they would show as down and fire alerts.

NATS has no Prometheus endpoint of its own and is not scraped yet. That would need its exporter as a sidecar.

## Service-level objectives

Two objectives over 30 days, in `templates/slo-rules.yaml`:

| Objective | Target | Measured from |
|---|---|---|
| Freshness | 99% of telemetry stored within 1 s of the ingester receiving it | `murmur_consumer_receive_to_commit_seconds`, the 1.024 s bucket |
| Delivery | 99.9% of packets the ingester receives reach the stream | queue drops plus publish errors, over packets received |

Packets lost on the radio link before they arrive are the drones' loss rather than the platform's, so delivery leaves them out; the ingester counts them as `frames_lost`.

The alerts follow the multiwindow, multi-burn-rate alerts of [Google's SRE workbook](https://sre.google/workbook/alerting-on-slos/). A burn rate of 1 spends the budget in exactly 30 days.

| Severity | Burn rate | Windows | Budget spent |
|---|---|---|---|
| page | 14.4 | 1 h and 5 m | 2% in an hour |
| page | 6 | 6 h and 30 m | 5% in six hours |
| ticket | 3 | 1 d and 2 h | 10% in a day |

Each alert needs both windows: the long one keeps a short blip from paging, and the short one clears the alert soon after its cause. The workbook's slowest alert, over 3 days, is left out because Prometheus here keeps 24 hours.

Two plain alerts cover what the objectives cannot see. A stopped consumer commits nothing, so it records no slow commits and freshness stays green; **MurmurBacklogNearRetention** fires instead when more than 5 minutes of traffic waits, halfway to JetStream's 10-minute retention. **MurmurTargetDown** fires when a Murmur pod stops answering `/metrics` for 2 minutes.

`make k8s-rules-test` renders the rules from the chart and runs [promtool](https://prometheus.io/docs/prometheus/latest/configuration/unit_testing_rules/) unit tests on them, in Docker and with no cluster: healthy traffic, an outage, a 2-minute blip that must not page, a stopped consumer and a lost target.

Alertmanager has no receiver configured, so alerts show in Prometheus and Alertmanager but notify nobody.

## In CI

The `Kubernetes (kind)` job in `.github/workflows/ci.yml` runs the same targets as a laptop: `helm lint`, the rule tests, `make kind-up`, `make k8s-up` and `make k8s-smoke`. The smoke test checks that:

- the three NATS servers formed one cluster, with the stream on all three;
- telemetry from the 10 fake drones reaches the database;
- the map and Grafana answer on their host ports;
- Prometheus scrapes all 7 Murmur pods and has loaded the 20 rules;
- KEDA's autoscaler is reading the backlog.

On failure the job prints the pods, recent events and each Murmur pod's logs.

## Not done

- **One database and one ingester.** A replicated or managed Postgres, and an ingester per site with commands routed to the one that hears each drone.
- **Secrets in values.** The passwords are local defaults in `values.yaml`. A real deployment would set them from outside git, for example with External Secrets or Sealed Secrets.
- **No TLS or Ingress.** The map is a plain NodePort; a real cluster would put it behind an Ingress with HTTPS.
- **A cloud cluster.** Only kind has been tried. The chart does not depend on kind, except that the edge node is found by the `murmur.io/edge` label, and a cloud load balancer would take the node ports' place.
