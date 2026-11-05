#!/usr/bin/env python3
"""Kills the ingester under load, then restarts it. Plan section 2: packets
sent while it is down are lost (UDP has no retry), and ingestion resumes within
5 seconds of the restart. Every drone must also keep its ID.

    loadtest/failures/kill_ingester.py [--drones 1000] [--down 10]
"""
import argparse
import time

import common
from common import INGESTER, PER_DRONE, metric

LIMIT = 5  # seconds, plan section 2

p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
p.add_argument("--drones", type=int, default=1000)
p.add_argument("--down", type=int, default=10, help="seconds the ingester stays down")
args = p.parse_args()

report = common.Report(f"kill_ingester ({args.drones} drones, {args.down} s)", f"ingestion resumes within {LIMIT} s of the restart")
expected = args.drones * PER_DRONE
with common.Fleet(args.drones, seconds=args.down + 45) as fleet:
    common.wait_for("every drone's link", lambda: metric(INGESTER, "murmur_ingester_links_up") == args.drones, 30)
    ids = common.sql("SELECT count(*) FROM drones")
    common.kill("ingester")
    time.sleep(args.down)

    restart = time.monotonic()  # before the start, so its time counts too
    common.start("ingester")
    common.wait_for("the first message published", lambda: (fleet.counter("published") or 0) > 0, 30, every=0.05)
    resumed = time.monotonic() - restart
    report.check(resumed <= LIMIT, f"first message published {resumed:.2f} s after the restart")

    # Heartbeats are 1 Hz: past the link timeout every drone has been heard.
    time.sleep(6)
    before = fleet.counter("published")
    time.sleep(5)
    rate = (fleet.counter("published") - before) / 5
    report.check(rate >= expected * 0.95, f"publishing {rate:,.0f} messages a second, of {expected:,} sent")
    up = int(metric(INGESTER, "murmur_ingester_links_up"))
    report.check(up == args.drones, f"{up} of {args.drones} links up")
    now = common.sql("SELECT count(*) FROM drones")
    report.check(now == ids, f"drones table {ids} before, {now} after: no drone got a new ID")

    sent, _ = fleet.finish()
    rows, _ = fleet.settled(300)
    report.note(f"sent {sent:,}, stored {rows:,}: {sent - rows:,} lost while it was down, as UDP allows")
report.finish()
