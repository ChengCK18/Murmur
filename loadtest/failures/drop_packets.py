#!/usr/bin/env python3
"""Loses a share of the packets at fakegen, which knows exactly how many went
missing. tc netem would need root and gives no count. Plan section 2 sets no
number here, so the test is that the ingester's loss counter tells the truth,
and that everything which did arrive is stored.

    loadtest/failures/drop_packets.py [--drones 100] [--seconds 60] [--loss 0.05]
"""
import argparse

import common
from common import PER_DRONE

p = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
p.add_argument("--drones", type=int, default=100)
p.add_argument("--seconds", type=int, default=60)
p.add_argument("--loss", type=float, default=0.05, help="share of messages lost")
args = p.parse_args()

report = common.Report(f"drop_packets ({args.drones} drones, {args.loss:.0%} lost)", "the loss counter matches the packets dropped; every packet that arrived is stored")
with common.Fleet(args.drones, messages=args.drones * PER_DRONE * args.seconds, drop=args.loss) as fleet:
    sent, dropped = fleet.finish()
    rows, _ = fleet.settled(120)
    lost, unregistered = fleet.added("frames_lost"), fleet.added("frames_unregistered")
    arrived = sent - dropped

    # A drop at a drone's very end leaves no gap to see, and one before its
    # first heartbeat is before the ingester knows it.
    report.check(abs(lost - dropped) <= args.drones, f"dropped {dropped:,}, counted {lost:,} (allowed to differ by up to one a drone)")
    report.check(rows == arrived - unregistered, f"arrived {arrived:,}, {unregistered:,} before a first heartbeat, stored {rows:,}")
    report.note(f"{fleet.added('link_losses')} link losses: three heartbeats lost in a row, which {args.loss:.0%} loss does cause now and then")
report.finish()
