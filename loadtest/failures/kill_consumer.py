#!/usr/bin/env python3
"""Kills the consumer under load, then restarts it. Plan section 2: zero
messages lost and zero duplicated if the outage is under 10 minutes.

    loadtest/failures/kill_consumer.py [--drones 1000] [--down 30]

--drones 100 --down 570 tests the edge of the retention window (20 minutes).
"""
import argparse

import common

p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
p.add_argument("--drones", type=int, default=1000)
p.add_argument("--down", type=int, default=30, help="seconds the consumer stays down")
args = p.parse_args()

report = common.Report(f"kill_consumer ({args.drones} drones, {args.down} s)", "zero messages lost, zero duplicated, outage under 10 minutes")
common.outage(report, "consumer", args.drones, args.down)
report.finish()
