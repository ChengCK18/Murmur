#!/usr/bin/env python3
"""Kills TimescaleDB under load, then restarts it, so it goes through crash
recovery. Plan section 2: zero messages lost and zero duplicated if the outage
is under 10 minutes.

    loadtest/failures/restart_database.py [--drones 1000] [--down 30]
"""
import argparse

import common

p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
p.add_argument("--drones", type=int, default=1000)
p.add_argument("--down", type=int, default=30, help="seconds the database stays down")
args = p.parse_args()

report = common.Report(f"restart_database ({args.drones} drones, {args.down} s)", "zero messages lost, zero duplicated, outage under 10 minutes")
common.outage(report, "timescaledb", args.drones, args.down)
report.finish()
