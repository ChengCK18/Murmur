#!/usr/bin/env python3
"""Load test (guide step 4.4): runs fakegen at rising drone counts against the
Compose stack, and saves throughput, loss and both latency spans from
Prometheus.

    make up
    loadtest/run.py before                    # 100, 250, 500 and 1,000 drones
    loadtest/run.py before --levels 1000      # one level

Results go to loadtest/results/<label>.json. Standard library only.
"""
import argparse
import json
import pathlib
import statistics
import subprocess
import time
import urllib.parse
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent
COMPOSE = ["docker", "compose", "-f", str(ROOT / "deploy/docker-compose.yml")]
PROMETHEUS = "http://127.0.0.1:9090"
LOAD = "murmur-load"
SERVICES = ["ingester", "consumer", "gateway", "timescaledb", "nats", "redis"]
SETTLE = 25  # s for registration and the first messages to pass through
SCRAPE = 5  # Prometheus scrape interval, s


def sh(*args, check=True):
    return subprocess.run(args, check=check, capture_output=True, text=True).stdout


def query(expr, at=None):
    """The first value of an instant query, or None."""
    params = {"query": expr}
    if at:
        params["time"] = at
    url = PROMETHEUS + "/api/v1/query?" + urllib.parse.urlencode(params)
    result = json.load(urllib.request.urlopen(url))["data"]["result"]
    return float(result[0]["value"][1]) if result else None


def quantile(q, metric, window, at, match=""):
    v = query(f"histogram_quantile({q}, sum(rate({metric}_bucket{{{match}}}[{window}s])) by (le))", at)
    return None if v is None or v != v else round(v * 1000, 1)  # ms; NaN: no samples


def cpu():
    """CPU % per service, from one docker stats sample."""
    names = [f"murmur-{s}-1" for s in SERVICES]
    out = sh("docker", "stats", "--no-stream", "--format", "{{.Name}} {{.CPUPerc}}", *names)
    return {l.split()[0].split("-")[1]: float(l.split()[1].rstrip("%")) for l in out.splitlines()}


def wait_for_lag(limit):
    """Seconds until the consumer has caught up, or None past limit."""
    start = time.time()
    while time.time() - start < limit:
        if (query("murmur_consumer_lag") or 0) < 50:
            return round(time.time() - start)
        time.sleep(SCRAPE)
    return None


def level(count, window, drain):
    print(f"--- {count} drones", flush=True)
    sh(*COMPOSE, "run", "-d", "--rm", "--no-deps", "--name", LOAD, "fakegen",
       f"-count={count}", "-target=ingester:14560")
    time.sleep(SETTLE)
    probe = subprocess.Popen([str(ROOT / "bin/wsprobe")], stdout=subprocess.PIPE, text=True)
    time.sleep(SCRAPE * 2)  # its first messages reach the metrics
    start = time.time()
    samples = []
    while time.time() - start < window:
        time.sleep(window / 4)
        samples.append(cpu())
    at = time.time()
    if at - start > window * 1.5:  # sleep() stops with the clock during a suspend
        raise SystemExit(f"the machine paused for {at - start - window:.0f} s: results not valid")
    w = f"[{window}s]"
    rate = lambda m: query(f"rate({m}{w})", at) or 0
    increase = lambda m: query(f"increase({m}{w})", at) or 0
    packets, lost = rate("murmur_ingester_packets_total"), rate("murmur_ingester_frames_lost_total")
    result = {
        "drones": count,
        "offered_per_s": count * 10,
        "received_per_s": round(packets),
        "published_per_s": round(rate("murmur_ingester_published_total")),
        "inserted_per_s": round(rate("murmur_consumer_inserted_total")),
        "gateway_feed_per_s": round(rate("murmur_gateway_feed_messages_total")),
        "browser_messages_per_s": round(rate("murmur_gateway_messages_sent_total")),
        "frames_lost_per_s": round(lost),
        "loss_pct": round(100 * max(0, count * 10 - packets) / (count * 10), 2),
        "queue_dropped": round(increase("murmur_ingester_queue_dropped_total")),
        "link_losses": round(increase("murmur_ingester_link_losses_total")),
        "publish_errors": round(increase("murmur_ingester_publish_errors_total")),
        "lag_at_end": round(query("murmur_consumer_lag", at) or 0),
        "commit_p50_ms": quantile(0.5, "murmur_consumer_receive_to_commit_seconds", window, at),
        "commit_p99_ms": quantile(0.99, "murmur_consumer_receive_to_commit_seconds", window, at),
        "send_p50_ms": quantile(0.5, "murmur_gateway_receive_to_send_seconds", window, at, 'subscription="drone"'),
        "send_p99_ms": quantile(0.99, "murmur_gateway_receive_to_send_seconds", window, at, 'subscription="drone"'),
        "area_send_p50_ms": quantile(0.5, "murmur_gateway_receive_to_send_seconds", window, at, 'subscription="area"'),
        "area_send_p99_ms": quantile(0.99, "murmur_gateway_receive_to_send_seconds", window, at, 'subscription="area"'),
        "cpu_pct": {s: round(statistics.mean(x[s] for x in samples if s in x)) for s in SERVICES},
    }
    probe.terminate()
    print("    wsprobe:", probe.communicate()[0].strip(), flush=True)
    sh("docker", "stop", LOAD)
    result["drain_s"] = wait_for_lag(drain)
    print("   ", json.dumps(result), flush=True)
    return result


def main():
    p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    p.add_argument("label", help="name of the results file")
    p.add_argument("--levels", type=int, nargs="+", default=[100, 250, 500, 1000])
    p.add_argument("--window", type=int, default=60, help="seconds measured at each level")
    p.add_argument("--drain", type=int, default=300, help="longest wait for the consumer to catch up")
    args = p.parse_args()

    sh("go", "build", "-o", str(ROOT / "bin/wsprobe"), "./loadtest/wsprobe")
    sh("docker", "rm", "-f", LOAD, check=False)
    sh(*COMPOSE, "stop", "fakegen")
    results = []
    try:
        for count in args.levels:
            time.sleep(5)  # past the link timeout: the fleet before it has gone
            results.append(level(count, args.window, args.drain))
    finally:
        sh("docker", "rm", "-f", LOAD, check=False)
        sh(*COMPOSE, "start", "fakegen")
        (ROOT / "loadtest/results").mkdir(exist_ok=True)
        path = ROOT / "loadtest/results" / f"{args.label}.json"
        path.write_text(json.dumps(results, indent=2) + "\n")
        print("saved", path)


if __name__ == "__main__":
    main()
