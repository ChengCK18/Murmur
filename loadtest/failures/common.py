"""What the failure tests (guide step 4.6) share. Each test runs against the
Compose stack (make up), swaps the 10 demo drones for its own fleet, kills
something, and prints pass or fail against the limit in plan section 2.

Containers are killed with SIGKILL rather than stopped, so nothing gets to
finish its work. Results are added to loadtest/results/failures.json.
"""
import json
import pathlib
import re
import subprocess
import sys
import time
import urllib.error
import urllib.request

ROOT = pathlib.Path(__file__).resolve().parent.parent.parent
COMPOSE = ["docker", "compose", "-f", str(ROOT / "deploy/docker-compose.yml")]
RESULTS = ROOT / "loadtest/results/failures.json"
LOAD = "murmur-load"
INGESTER, CONSUMER = 9101, 9102  # metrics ports
PER_DRONE = 10  # messages a second


def sh(*args, check=True):
    return subprocess.run(args, check=check, capture_output=True, text=True).stdout.strip()


def metric(port, name):
    """A metric's current value, or None if the service is not answering."""
    try:
        body = urllib.request.urlopen(f"http://127.0.0.1:{port}/metrics", timeout=2).read().decode()
    except (urllib.error.URLError, ConnectionError, TimeoutError):
        return None
    found = re.search(rf"^{name}(?:{{.*}})? (\S+)$", body, re.M)
    return float(found.group(1)) if found else 0.0


def sql(query):
    return sh("docker", "exec", "murmur-timescaledb-1", "psql", "-U", "murmur", "-d", "murmur", "-tAc", query)


def wait_for(what, ready, limit, every=0.5):
    """Seconds until ready() is true; raises past limit."""
    start = time.monotonic()
    while time.monotonic() - start < limit:
        try:
            if ready():
                return time.monotonic() - start
        except subprocess.CalledProcessError:
            pass  # the database is not back yet
        time.sleep(every)
    raise TimeoutError(f"gave up after {limit} s waiting for {what}")


def kill(service):
    """SIGKILL, and make sure it stays down until start()."""
    name = f"murmur-{service}-1"
    sh("docker", "kill", name)
    time.sleep(1)
    if sh("docker", "inspect", "-f", "{{.State.Running}}", name) == "true":
        raise RuntimeError(f"{name} came back by itself, so the outage is not the length asked for")


def start(service):
    sh("docker", "start", f"murmur-{service}-1")


class Fleet:
    """A fakegen run in place of the demo drones. Inside the with block,
    stored() counts only this fleet's rows."""

    def __init__(self, drones, messages=0, seconds=0, drop=0.0):
        self.drones = drones
        self.args = [f"-count={drones}", "-target=ingester:14560", f"-drop={drop}"]
        if messages:
            self.args.append(f"-messages={messages}")
        if seconds:
            self.args.append(f"-duration={seconds}s")

    def __enter__(self):
        sh("docker", "rm", "-f", LOAD, check=False)
        sh(*COMPOSE, "stop", "fakegen")
        wait_for("the consumer to catch up", lambda: metric(CONSUMER, "murmur_consumer_lag") == 0, 300)
        time.sleep(5)  # past the link timeout, so old drone IDs can be rebound
        self.since = sql("SELECT now()")
        self.counters = {n: self.counter(n) for n in ("published", "publish_errors", "frames_unregistered", "frames_lost", "link_losses")}
        sh(*COMPOSE, "run", "-d", "--no-deps", "--name", LOAD, "fakegen", *self.args)
        return self

    def __exit__(self, *_):
        sh("docker", "rm", "-f", LOAD, check=False)
        sh(*COMPOSE, "up", "-d", "--wait")

    def counter(self, name):
        return metric(INGESTER, f"murmur_ingester_{name}_total")

    def added(self, name):
        """How much an ingester counter has grown since the fleet started."""
        return int(self.counter(name) - self.counters[name])

    def stored(self):
        return int(sql(f"SELECT count(*) FROM telemetry WHERE received_at > '{self.since}'"))

    def finish(self):
        """Waits for fakegen to end; returns (sent, dropped)."""
        sh("docker", "wait", LOAD)
        logs = subprocess.run(["docker", "logs", LOAD], check=True, capture_output=True, text=True).stderr  # log.Printf
        line = re.search(r"sent (\d+) messages, (\d+) send errors, (\d+) dropped", logs)
        if not line:
            raise RuntimeError("fakegen did not report what it sent")
        sent, errors, dropped = map(int, line.groups())
        if errors:
            raise RuntimeError(f"fakegen had {errors} send errors")
        return sent, dropped

    def settled(self, limit):
        """Waits for the stream to empty and the row count to stop moving;
        returns (rows, seconds taken)."""
        last = {"rows": -1}

        def still():
            rows = self.stored()
            done = rows == last["rows"] and metric(CONSUMER, "murmur_consumer_lag") == 0
            last["rows"] = rows
            return done

        took = wait_for("the rows to stop changing", still, limit, every=3)
        return last["rows"], took


class Report:
    def __init__(self, name, limit):
        self.name, self.limit, self.lines, self.passed = name, limit, [], True
        print(f"=== {name}\n    limit: {limit}", flush=True)

    def note(self, line):
        self.lines.append(line)
        print(f"    {line}", flush=True)

    def check(self, ok, line):
        self.passed = self.passed and ok
        self.lines.append(("ok: " if ok else "FAILED: ") + line)
        print(f"  {'ok' if ok else 'FAILED'}  {line}", flush=True)

    def finish(self):
        print("PASS" if self.passed else "FAIL", self.name, flush=True)
        results = json.loads(RESULTS.read_text()) if RESULTS.exists() else {}
        results[self.name] = {"passed": self.passed, "limit": self.limit, "lines": self.lines}
        RESULTS.write_text(json.dumps(results, indent=2) + "\n")
        sys.exit(0 if self.passed else 1)


def outage(report, service, drones, down):
    """Kills a service under load for down seconds and checks that every
    message sent ends up stored exactly once. The table's unique key rules out
    a duplicate row, so a count equal to the messages sent means none is
    missing either."""
    messages = drones * PER_DRONE * (down + 30)  # flying for the whole outage and after it
    with Fleet(drones, messages=messages) as fleet:
        wait_for("rows from the fleet", lambda: fleet.stored() >= drones * PER_DRONE * 10, 60)
        kill(service)
        if service == "consumer":
            at_kill = fleet.stored()
            report.note(f"killed at {at_kill:,} rows, down for {down} s at {drones * PER_DRONE:,} messages a second")
        time.sleep(down)
        if service == "consumer":
            report.check(fleet.stored() == at_kill, "no rows written during the outage")
        start(service)
        if service == "timescaledb":
            took = wait_for("the database", lambda: sql("SELECT 1") == "1", 120)
            report.note(f"database answering {took:.1f} s after its restart")
            wait_for("the consumer to write again", lambda: fleet.stored() > 0, 60)
            report.check((metric(CONSUMER, "murmur_consumer_failed_batches_total") or 0) > 0, "the consumer saw the outage: batches failed and were left for redelivery")
        sent, _ = fleet.finish()
        rows, took = fleet.settled(max(600, down * 4))
        unregistered = fleet.added("frames_unregistered")
        errors = fleet.added("publish_errors")
        report.note(f"caught up {took:.0f} s after the last message; {int(metric(CONSUMER, 'murmur_consumer_duplicates_total') or 0):,} redelivered rows dropped as duplicates")
        report.check(errors == 0, f"{errors} publish errors")
        report.check(unregistered == 0, f"{unregistered} frames sent before a drone's first heartbeat")
        report.check(rows == sent, f"sent {sent:,}, stored {rows:,}: {sent - rows:,} missing")
