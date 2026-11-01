#!/usr/bin/env python3
"""Fly scripted SITL sessions and record them as .tlog files for testdata/ (guide step 0.6).

Usage, inside ~/venv-ardupilot:
    python deploy/sitl/record.py idle|flight|restart [-o FILE] [--force]

  idle     boot, sit disarmed for 2 minutes
  flight   take off, fly testdata/missions/square.txt in AUTO, return to launch, land
  restart  take off and fly, stop SITL mid-air (SIGINT, like Ctrl-C), start it again, fly again, land

The script runs its own headless arducopter (instance 9 by default, so it does not clash with a SITL
you already have open) that sends straight to this script over UDP, as in mode C. The script is the
ground station (system ID 255, component ID 190): it sends heartbeats, sets modes, arms and uploads the
mission. Every message in both directions is written in MAVProxy's .tlog format: an 8-byte big-endian
timestamp (microseconds since 1970) followed by one MAVLink frame.
"""
import argparse
import os
import signal
import struct
import subprocess
import sys
import time
from pathlib import Path

os.environ.setdefault("MAVLINK20", "1")
from pymavlink import mavutil, mavwp  # noqa: E402

mavlink = mavutil.mavlink

ROOT = Path(__file__).resolve().parents[2]
AP = Path(os.environ.get("ARDUPILOT", Path.home() / "ardupilot"))
HOME_LAT, HOME_LNG, HOME_ALT = -35.363262, 149.165237, 584
MISSION = ROOT / "testdata/missions/square.txt"
MODES = {"STABILIZE": 0, "AUTO": 3, "GUIDED": 4, "LOITER": 5, "RTL": 6, "LAND": 9}
MODE_NAMES = {v: k for k, v in MODES.items()}


class Session:
    def __init__(self, tlog_path, instance, port):
        self.instance = instance
        self.port = port
        self.dir = ROOT / f".sitl/record-{instance}"
        self.dir.mkdir(parents=True, exist_ok=True)
        self.tlog = open(tlog_path, "wb")
        self.conn = mavutil.mavlink_connection(
            f"udpin:127.0.0.1:{port}", source_system=255, source_component=190, udp_timeout=5)
        self.sitl = None
        self.last_hb_sent = 0.0
        self.armed = False
        self.mode = None
        self.rel_alt = 0.0
        self.ekf_flags = 0
        self.mission_seq = None
        self.counts = {"in": 0, "out": 0}

    # --- simulator process ---

    def start_sitl(self, wipe):
        cmd = [str(AP / "build/sitl/bin/arducopter"), "--model", "+", "-I", str(self.instance),
               "--sysid", "1", "--home", f"{HOME_LAT},{HOME_LNG},{HOME_ALT},0",
               "--defaults", f"{AP}/Tools/autotest/default_params/copter.parm,"
                             f"{ROOT}/deploy/sitl/murmur-streams.parm",
               "--serial0", f"udpclient:127.0.0.1:{self.port}"]
        if wipe:
            cmd.append("-w")
        log = open(self.dir / "sitl.log", "ab")
        self.sitl = subprocess.Popen(cmd, cwd=self.dir, stdout=log, stderr=subprocess.STDOUT)
        say(f"started SITL (pid {self.sitl.pid}{', wiped' if wipe else ''})")
        self.wait_until(lambda: self.mode is not None, 60, "first heartbeat")

    def stop_sitl(self):
        if self.sitl and self.sitl.poll() is None:
            self.sitl.send_signal(signal.SIGINT)
            try:
                self.sitl.wait(10)
            except subprocess.TimeoutExpired:
                self.sitl.kill()
                self.sitl.wait()
            say("stopped SITL")
        self.sitl = None
        self.mode = None
        self.armed = False
        self.ekf_flags = 0

    # --- message pump ---

    def write_tlog(self, msg):
        self.tlog.write(struct.pack(">Q", int(time.time() * 1e6)) + msg.get_msgbuf())

    def send(self, msg):
        self.conn.mav.send(msg)
        if self.conn.clients:  # nothing is sent before the simulator has been heard from
            self.write_tlog(msg)
            self.counts["out"] += 1

    def recv(self, timeout=0.1):
        now = time.time()
        if now - self.last_hb_sent >= 1:
            self.send(self.conn.mav.heartbeat_encode(
                mavlink.MAV_TYPE_GCS, mavlink.MAV_AUTOPILOT_INVALID, 0, 0, mavlink.MAV_STATE_ACTIVE))
            self.last_hb_sent = now
        msg = self.conn.recv_match(blocking=True, timeout=timeout)
        if msg is None or msg.get_type() == "BAD_DATA":  # boot text lands here
            return None
        self.write_tlog(msg)
        self.counts["in"] += 1
        t = msg.get_type()
        if t == "HEARTBEAT" and msg.get_srcComponent() == 1 and msg.type != mavlink.MAV_TYPE_GCS:
            self.armed = bool(msg.base_mode & mavlink.MAV_MODE_FLAG_SAFETY_ARMED)
            self.mode = MODE_NAMES.get(msg.custom_mode, msg.custom_mode)
        elif t == "GLOBAL_POSITION_INT":
            self.rel_alt = msg.relative_alt / 1000
        elif t == "EKF_STATUS_REPORT":
            self.ekf_flags = msg.flags
        elif t == "STATUSTEXT":
            say(f"AP: {msg.text}")
        elif t == "MISSION_CURRENT" and self.mode == "AUTO" and msg.seq != self.mission_seq:
            self.mission_seq = msg.seq
            say(f"mission item {msg.seq}")
        return msg

    def run_for(self, seconds):
        end = time.time() + seconds
        while time.time() < end:
            self.recv()

    def wait_until(self, cond, timeout, what):
        end = time.time() + timeout
        while not cond():
            if time.time() > end:
                raise RuntimeError(f"timed out after {timeout}s waiting for {what}")
            self.recv()

    def wait_msg(self, pred, timeout):
        end = time.time() + timeout
        while time.time() < end:
            msg = self.recv()
            if msg is not None and pred(msg):
                return msg
        return None

    # --- ground-station actions ---

    def command(self, cmd, *params, timeout=3):
        p = list(params) + [0] * (7 - len(params))
        self.send(self.conn.mav.command_long_encode(1, 1, cmd, 0, *p))
        ack = self.wait_msg(lambda m: m.get_type() == "COMMAND_ACK" and m.command == cmd, timeout)
        return ack.result if ack else None

    def set_mode(self, name):
        self.command(mavlink.MAV_CMD_DO_SET_MODE, mavlink.MAV_MODE_FLAG_CUSTOM_MODE_ENABLED, MODES[name])
        self.wait_until(lambda: self.mode == name, 10, f"mode {name}")
        say(f"mode {name}")

    def wait_position(self, timeout=180):
        # GPS lock and the EKF switching to it take about 30-60 s after boot. Arming can be accepted
        # before that, but takeoff is then rejected, so wait for an absolute horizontal position.
        self.wait_until(lambda: self.ekf_flags & mavlink.EKF_POS_HORIZ_ABS
                        and not self.ekf_flags & mavlink.EKF_CONST_POS_MODE, timeout, "EKF using GPS")
        say("position estimate ready")

    def arm(self, timeout=60):
        end = time.time() + timeout
        while self.command(mavlink.MAV_CMD_COMPONENT_ARM_DISARM, 1) != mavlink.MAV_RESULT_ACCEPTED:
            if time.time() > end:
                raise RuntimeError("could not arm")
            self.run_for(3)
        self.wait_until(lambda: self.armed, 5, "armed")
        say("armed")

    def takeoff(self, alt):
        if self.command(mavlink.MAV_CMD_NAV_TAKEOFF, 0, 0, 0, 0, 0, 0, alt) != mavlink.MAV_RESULT_ACCEPTED:
            raise RuntimeError("takeoff rejected")
        self.wait_until(lambda: self.rel_alt >= alt * 0.95, 60, f"altitude {alt} m")
        say(f"at {self.rel_alt:.1f} m")

    def fly_to(self, lat, lng, alt, seconds):
        self.send(self.conn.mav.set_position_target_global_int_encode(
            0, 1, 1, mavlink.MAV_FRAME_GLOBAL_RELATIVE_ALT_INT, 0b110111111000,
            int(lat * 1e7), int(lng * 1e7), alt, 0, 0, 0, 0, 0, 0, 0, 0))
        say(f"flying to {lat:.6f},{lng:.6f}")
        self.run_for(seconds)

    def upload_mission(self, path, timeout=60):
        # Straight after boot the mission store is not ready and answers MAV_MISSION_NO_SPACE, so retry.
        end = time.time() + timeout
        while (result := self.try_upload_mission(path)) != mavlink.MAV_MISSION_ACCEPTED:
            if time.time() > end:
                raise RuntimeError(f"mission rejected: {result}")
            self.run_for(2)
        say(f"uploaded mission {path.name}")

    def try_upload_mission(self, path):
        wp = mavwp.MAVWPLoader(target_system=1, target_component=1)
        n = wp.load(str(path))
        self.send(self.conn.mav.mission_count_encode(1, 1, n, mavlink.MAV_MISSION_TYPE_MISSION))
        while True:
            msg = self.wait_msg(lambda m: m.get_type() in
                                ("MISSION_REQUEST_INT", "MISSION_REQUEST", "MISSION_ACK"), 5)
            if msg is None:
                return None
            if msg.get_type() == "MISSION_ACK":
                return msg.type
            w = wp.wp(msg.seq)
            self.send(self.conn.mav.mission_item_int_encode(
                1, 1, w.seq, w.frame, w.command, w.current, w.autocontinue,
                w.param1, w.param2, w.param3, w.param4, int(w.x * 1e7), int(w.y * 1e7), w.z,
                mavlink.MAV_MISSION_TYPE_MISSION))

    def wait_landed(self, timeout=600):
        self.wait_until(lambda: not self.armed, timeout, "landing and disarm")
        say("landed and disarmed")

    def close(self):
        self.stop_sitl()
        self.tlog.close()


def take_off(s):
    s.wait_position()
    s.set_mode("GUIDED")
    s.arm()
    s.takeoff(20)


def scenario_idle(s):
    s.start_sitl(wipe=True)
    s.run_for(120)


def scenario_flight(s):
    s.start_sitl(wipe=True)
    s.upload_mission(MISSION)
    take_off(s)
    s.set_mode("AUTO")
    s.wait_landed()
    s.run_for(5)


def scenario_restart(s):
    s.start_sitl(wipe=True)
    take_off(s)
    s.fly_to(HOME_LAT + 0.0008, HOME_LNG, 20, 20)
    s.stop_sitl()  # mid-air, like Ctrl-C
    s.run_for(5)
    s.start_sitl(wipe=False)
    take_off(s)
    s.fly_to(HOME_LAT, HOME_LNG + 0.0008, 20, 20)
    s.set_mode("RTL")
    s.wait_landed()
    s.run_for(5)


SCENARIOS = {"idle": scenario_idle, "flight": scenario_flight, "restart": scenario_restart}
START = time.time()


def say(text):
    print(f"[{time.time() - START:6.1f}s] {text}", flush=True)


def main():
    ap = argparse.ArgumentParser(description=__doc__.split("\n")[0])
    ap.add_argument("scenario", choices=SCENARIOS)
    ap.add_argument("-o", "--output", type=Path, help="default: testdata/<scenario>.tlog")
    ap.add_argument("--force", action="store_true", help="overwrite an existing recording")
    ap.add_argument("--instance", type=int, default=9, help="SITL instance number (-I)")
    ap.add_argument("--port", type=int, default=14590, help="UDP port SITL sends to")
    args = ap.parse_args()

    out = args.output or ROOT / f"testdata/{args.scenario}.tlog"
    if out.exists() and not args.force:
        sys.exit(f"{out} already exists; use --force to overwrite")
    out.parent.mkdir(parents=True, exist_ok=True)

    s = Session(out, args.instance, args.port)
    try:
        SCENARIOS[args.scenario](s)
    except KeyboardInterrupt:
        say("interrupted; recording kept up to here")
    finally:
        s.close()
    say(f"wrote {out}: {s.counts['in']} messages in, {s.counts['out']} out, "
        f"{out.stat().st_size / 1e6:.1f} MB")


if __name__ == "__main__":
    main()
