import { useCallback, useEffect, useRef, useState } from "react";
import { CommandPanel } from "./CommandPanel";
import { Commands, type FleetView } from "./commands";
import { Connection, type Status, wsURL } from "./connection";
import { Drones } from "./drones";
import { DroneList } from "./DroneList";
import { DroneMap } from "./DroneMap";
import { FleetBar } from "./FleetBar";
import type { BBox, CommandParams, CommandType, CommandUpdate, DroneState } from "./types";

const LIST_EVERY = 10_000; // ms between refreshes of the whole fleet's list

export function App() {
  const [drones, setDrones] = useState<DroneState[]>([]);
  const [status, setStatus] = useState<Status>("connecting");
  const [selected, setSelected] = useState<number | null>(null);
  const store = useRef(new Drones());
  const conn = useRef<Connection | null>(null);
  const area = useRef<BBox | null>(null);
  const commands = useRef(new Commands());
  const [user, setUser] = useState("");
  const [log, setLog] = useState<CommandUpdate[]>([]);
  const [checked, setChecked] = useState(false); // the login is known
  const selectedRef = useRef<number | null>(null);
  selectedRef.current = selected;

  const [fleet, setFleet] = useState<FleetView | null>(null);
  // The latest fleet command: what the summary is counted from.
  const latestFleet = useRef<{ type: string; count: number; requestId: string; fleetId?: string; error?: string } | null>(null);

  const showLog = useCallback(() => {
    setLog(selectedRef.current === null ? [] : commands.current.forDrone(selectedRef.current));
    const f = latestFleet.current;
    setFleet(f ? commands.current.fleet(f.type, f.count, f.fleetId, f.error) : null);
  }, []);

  // A fleet's statuses, reloaded after a reconnect: any sent meanwhile went by
  // unseen, and the summary must not wait for them for ever.
  const reloadFleet = useCallback(async () => {
    const id = latestFleet.current?.fleetId;
    if (!id) return;
    try {
      const res = await fetch(`/api/fleets/${id}/commands`);
      if (!res.ok) return;
      const body = (await res.json()) as { commands: CommandUpdate[] };
      commands.current.load(body.commands);
      showLog();
    } catch {
      // The gateway is down; the next reconnect tries again.
    }
  }, [showLog]);

  // A drone's commands from the command service: on selecting it, and after a
  // reconnect, when statuses may have gone by unseen.
  const reloadLog = useCallback(
    async (id: number) => {
      try {
        const res = await fetch(`/api/drones/${id}/commands`);
        if (!res.ok) return;
        const body = (await res.json()) as { commands: CommandUpdate[] };
        commands.current.load(body.commands);
        showLog();
      } catch {
        // The gateway is down; the banner shows it.
      }
    },
    [showLog],
  );

  useEffect(() => {
    // Updates arrive up to 4 per second per drone; render at most once per frame.
    let frame = 0;
    const render = () => {
      if (frame) return;
      frame = requestAnimationFrame(() => {
        frame = 0;
        setDrones(store.current.list());
      });
    };

    const c = new Connection(wsURL(), {
      status: (st) => {
        setStatus(st);
        if (st === "reconnecting") {
          commands.current.connectionLost();
          showLog();
        }
        if (st === "open") {
          if (selectedRef.current !== null) void reloadLog(selectedRef.current);
          void reloadFleet();
        }
      },
      fleetStarted: (started) => {
        const f = latestFleet.current;
        if (!f || f.requestId !== started.request_id) return;
        f.fleetId = started.fleet_id;
        f.error = started.error;
        if (started.count) f.count = started.count;
        showLog();
      },
      commandStatus: (u, requestId) => {
        commands.current.apply(u, requestId);
        showLog();
      },
      snapshot: (ds) => {
        store.current.snapshot(ds);
        render();
      },
      update: (d) => {
        store.current.update(d);
        render();
      },
      remove: (id) => {
        store.current.remove(id);
        c.unsubscribe([id]);
        render();
      },
      error: (msg) => console.warn("gateway:", msg),
    });

    conn.current = c;
    if (area.current) c.area(area.current); // the map mounts first

    // The list shows the whole fleet; only drones in the map area, and the
    // selected one, are live between refreshes.
    const refresh = async () => {
      try {
        const res = await fetch("/api/drones");
        if (!res.ok) return;
        const body = (await res.json()) as { drones: DroneState[] };
        c.unsubscribe(store.current.merge(body.drones));
        render();
      } catch {
        // The gateway is down; the banner shows it and the next refresh retries.
      }
    };

    c.start();
    refresh();
    const timer = setInterval(refresh, LIST_EVERY);
    return () => {
      clearInterval(timer);
      cancelAnimationFrame(frame);
      c.stop();
      conn.current = null;
    };
  }, [reloadLog, reloadFleet, showLog]);

  useEffect(() => {
    void (async () => {
      try {
        const res = await fetch("/api/session");
        if (res.ok) setUser(((await res.json()) as { user: string }).user);
      } catch {
        // The gateway is down: not logged in, as far as this page can tell.
      }
      setChecked(true);
    })();
  }, []);

  // The selected drone stays live wherever the map is.
  useEffect(() => {
    showLog();
    if (selected === null) return;
    if (user) void reloadLog(selected);
    const c = conn.current;
    c?.subscribe([selected]);
    return () => c?.unsubscribe([selected]);
  }, [selected, user, reloadLog, showLog]);

  const select = useCallback((id: number) => setSelected(id), []);
  const followArea = useCallback((b: BBox) => {
    area.current = b;
    conn.current?.area(b);
  }, []);

  const send = useCallback(
    (type: CommandType, params: CommandParams = {}) => {
      const id = selectedRef.current;
      if (id === null) return;
      const requestId = commands.current.start(id, type, user);
      if (!conn.current?.command(requestId, id, type, params)) {
        commands.current.notSent(requestId);
      }
      showLog();
    },
    [user, showLog],
  );

  const sendFleet = useCallback(
    (type: CommandType) => {
      const ids = store.current.list().flatMap((d) => (d.link === "ok" ? [d.drone_id] : []));
      const requestId = `fleet-${Date.now()}`;
      latestFleet.current = { type, count: ids.length, requestId };
      if (!conn.current?.fleetCommand(requestId, ids, type)) {
        latestFleet.current.error = "Not connected to the gateway";
      }
      showLog();
    },
    [showLog],
  );

  const login = useCallback(async (username: string, password: string): Promise<string> => {
    try {
      const res = await fetch("/api/login", {
        method: "POST",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ username, password }),
      });
      const body = (await res.json()) as { user?: string; error?: string };
      if (!res.ok || !body.user) return body.error ?? "Could not log in";
      setUser(body.user);
      conn.current?.reconnect();
      return "";
    } catch {
      return "Could not reach the gateway";
    }
  }, []);

  const logout = useCallback(async () => {
    try {
      await fetch("/api/logout", { method: "POST" });
    } catch {
      return; // still logged in as far as the gateway knows
    }
    setUser("");
    commands.current.clear();
    latestFleet.current = null;
    showLog();
    conn.current?.reconnect();
  }, [showLog]);

  const drone = drones.find((d) => d.drone_id === selected);

  return (
    <div className="app">
      {status !== "open" && (
        <div className="banner" data-testid="banner">
          {status === "connecting" ? "Connecting…" : "Connection lost, reconnecting…"}
        </div>
      )}
      <DroneList drones={drones} selected={selected} onSelect={select}>
        {user && <FleetBar live={drones.filter((d) => d.link === "ok").length} view={fleet} onSend={sendFleet} />}
      </DroneList>
      <div className="stage">
        <DroneMap drones={drones} selected={selected} onSelect={select} onArea={followArea} />
        {drone && checked && (
          <CommandPanel key={drone.drone_id} drone={drone} user={user} log={log} onSend={send} onLogin={login} onLogout={logout} />
        )}
      </div>
    </div>
  );
}
