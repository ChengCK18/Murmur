#!/usr/bin/env python3
"""Kills the gateway with a real browser connected, then restarts it. Plan
section 2: browsers reconnect automatically and resume live updates. The
browser's reconnect code is what is under test, so this drives Chrome, with the
Phase 3 check in web/e2e/map.spec.ts, against the demo fleet.
"""
import subprocess

import common

report = common.Report("kill_gateway", "browsers reconnect automatically and resume live updates")
run = subprocess.run(["npx", "playwright", "test", "--reporter=line", "-g", "gateway restart"], cwd=common.ROOT / "web", capture_output=True, text=True)
report.check(run.returncode == 0, "banner shown, then gone by itself; the same markers moving and all live")
if run.returncode:
    print(run.stdout[-2000:])
report.finish()
