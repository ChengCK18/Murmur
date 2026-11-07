import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { backoff, Connection, type Handlers, type SocketLike, type Status } from "./connection";
import type { DroneState } from "./types";

class FakeSocket implements SocketLike {
  readyState = 0;
  sent: unknown[] = [];
  onopen: (() => void) | null = null;
  onmessage: ((ev: { data: unknown }) => void) | null = null;
  onclose: (() => void) | null = null;
  send(data: string) {
    this.sent.push(JSON.parse(data));
  }
  close() {
    this.readyState = 3;
    this.onclose?.();
  }
  // Test controls.
  accept() {
    this.readyState = 1;
    this.onopen?.();
  }
  receive(msg: unknown) {
    this.onmessage?.({ data: JSON.stringify(msg) });
  }
  drop() {
    this.close();
  }
}

function drone(id: number): DroneState {
  return { tenant_id: 1, drone_id: id, sys_id: id, armed: true, link: "ok", last_seen: "", last_heartbeat: "" };
}

describe("backoff", () => {
  it("waits 1, 2, 4, 8 and then 10 seconds", () => {
    expect([0, 1, 2, 3, 4, 5, 9].map(backoff)).toEqual([1000, 2000, 4000, 8000, 10000, 10000, 10000]);
  });
});

describe("Connection", () => {
  let sockets: FakeSocket[];
  let statuses: Status[];
  let snapshots: DroneState[][];
  let updates: DroneState[];
  let removed: number[];
  let conn: Connection;

  beforeEach(() => {
    vi.useFakeTimers();
    sockets = [];
    statuses = [];
    snapshots = [];
    updates = [];
    removed = [];
    const h: Handlers = {
      status: (s) => statuses.push(s),
      snapshot: (d) => snapshots.push(d),
      update: (d) => updates.push(d),
      remove: (id) => removed.push(id),
    };
    conn = new Connection("ws://test/ws", h, () => {
      const s = new FakeSocket();
      sockets.push(s);
      return s;
    });
  });
  afterEach(() => vi.useRealTimers());

  it("subscribes on open and dispatches messages", () => {
    conn.subscribe([1, 2]); // before the socket is open: sent on open
    conn.start();
    sockets[0].accept();
    expect(sockets[0].sent).toEqual([{ type: "subscribe", drones: [1, 2] }]);

    conn.subscribe([2, 3]); // only the new drone is sent
    expect(sockets[0].sent[1]).toEqual({ type: "subscribe", drones: [3] });

    sockets[0].receive({ type: "snapshot", drones: [drone(1)] });
    sockets[0].receive({ type: "update", drone_id: 2, state: drone(2) });
    sockets[0].onmessage?.({ data: "not json" }); // ignored
    expect(snapshots).toEqual([[drone(1)]]);
    expect(updates).toEqual([drone(2)]);
    expect(statuses).toEqual(["connecting", "open"]);
  });

  it("reconnects with backoff and subscribes to everything again", () => {
    conn.subscribe([1, 2, 3]);
    conn.start();
    sockets[0].accept();
    sockets[0].drop(); // the gateway restarted
    expect(statuses.at(-1)).toBe("reconnecting");

    // Attempts 1, 2 and 3 fail: after 1 s, then 2 s, then 4 s.
    for (const [i, wait] of [[1, 1000], [2, 2000], [3, 4000]] as const) {
      vi.advanceTimersByTime(wait - 1);
      expect(sockets.length).toBe(i);
      vi.advanceTimersByTime(1);
      expect(sockets.length).toBe(i + 1);
      sockets[i].drop();
    }
    // Attempt 4 succeeds after 8 s: it asks for all three drones again.
    vi.advanceTimersByTime(8000);
    sockets[4].accept();
    expect(sockets[4].sent).toEqual([{ type: "subscribe", drones: [1, 2, 3] }]);
    expect(statuses.at(-1)).toBe("open");

    // Success resets the backoff: the next drop retries after 1 s again.
    sockets[4].drop();
    vi.advanceTimersByTime(1000);
    expect(sockets.length).toBe(6);
  });

  it("removes and unsubscribes", () => {
    conn.subscribe([1, 2]);
    conn.start();
    sockets[0].accept();
    sockets[0].receive({ type: "remove", drone_id: 2 });
    expect(removed).toEqual([2]);

    conn.unsubscribe([2, 9]); // 9 was never subscribed
    expect(sockets[0].sent.at(-1)).toEqual({ type: "unsubscribe", drones: [2] });
    // After a reconnect only drone 1 is asked for.
    sockets[0].drop();
    vi.advanceTimersByTime(1000);
    sockets[1].accept();
    expect(sockets[1].sent).toEqual([{ type: "subscribe", drones: [1] }]);
  });

  it("follows a map area, and asks for it again after a reconnect", () => {
    conn.area([1, 2, 3, 4]); // before the socket is open: sent on open
    conn.subscribe([7]);
    conn.start();
    sockets[0].accept();
    expect(sockets[0].sent).toEqual([
      { type: "subscribe_area", bbox: [1, 2, 3, 4] },
      { type: "subscribe", drones: [7] },
    ]);

    conn.area([5, 6, 7, 8]); // replaces the last one
    expect(sockets[0].sent.at(-1)).toEqual({ type: "subscribe_area", bbox: [5, 6, 7, 8] });

    sockets[0].drop();
    vi.advanceTimersByTime(1000);
    sockets[1].accept();
    expect(sockets[1].sent[0]).toEqual({ type: "subscribe_area", bbox: [5, 6, 7, 8] });
  });

  it("stops for good", () => {
    conn.start();
    sockets[0].accept();
    conn.stop();
    vi.advanceTimersByTime(60_000);
    expect(sockets.length).toBe(1);
    expect(statuses.at(-1)).toBe("open"); // no "reconnecting" after a deliberate stop
  });

  it("sends a command only on an open socket", () => {
    expect(conn.command("r1", 1, "arm")).toBe(false);
    conn.start();
    expect(conn.command("r1", 1, "arm")).toBe(false);
    sockets[0].accept();
    expect(conn.command("r2", 7, "takeoff", { altitude_m: 20 })).toBe(true);
    expect(sockets[0].sent.at(-1)).toEqual({ type: "command", request_id: "r2", drone_id: 7, command: "takeoff", params: { altitude_m: 20 } });
  });

  it("reconnects at once for a new login, without showing an outage", () => {
    conn.subscribe([1]);
    conn.start();
    sockets[0].accept();
    statuses.length = 0;
    conn.reconnect();
    expect(sockets).toHaveLength(2);
    expect(sockets[0].readyState).toBe(3);
    sockets[1].accept();
    expect(statuses).toEqual(["open"]);
    expect(sockets[1].sent).toEqual([{ type: "subscribe", drones: [1] }]);
    vi.advanceTimersByTime(60_000);
    expect(sockets).toHaveLength(2);
  });

  it("sends a fleet command only on an open socket", () => {
    expect(conn.fleetCommand("r1", [1, 2], "land")).toBe(false);
    conn.start();
    sockets[0].accept();
    expect(conn.fleetCommand("r2", [1, 2], "land")).toBe(true);
    expect(sockets[0].sent.at(-1)).toEqual({ type: "fleet_command", request_id: "r2", drones: [1, 2], command: "land", params: {} });
  });

  it("sends a mission only on an open socket", () => {
    const points = [{ lat: -35, lon: 149 }];
    expect(conn.upload("r1", 1, points, 20)).toBe(false);
    conn.start();
    sockets[0].accept();
    expect(conn.upload("r2", 1, points, 20)).toBe(true);
    expect(sockets[0].sent.at(-1)).toEqual({ type: "mission_upload", request_id: "r2", drone_id: 1, waypoints: points, altitude_m: 20 });
  });
});
