import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { AlertList } from "./AlertList";
import { Alerts } from "./alerts";
import { CommandPanel } from "./CommandPanel";
import { Commands, type FleetView } from "./commands";
import { Connection, type Status, wsURL } from "./connection";
import { Drones } from "./drones";
import { DroneList } from "./DroneList";
import { DroneMap } from "./DroneMap";
import { FleetBar } from "./FleetBar";
import { type Flight, flight as toFlight, type History, pointAt, track } from "./history";
import { Replay } from "./Replay";
import type { Alert, BBox, CommandParams, CommandType, CommandUpdate, DroneState, Fence, Mission, Waypoint } from "./types";

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

  const [mission, setMission] = useState<Mission | null>(null);
  const alertStore = useRef(new Alerts());
  const [alerts, setAlerts] = useState<Alert[]>([]);
  const [alerted, setAlerted] = useState<Set<number>>(new Set());
  const [fence, setFence] = useState<Waypoint[]>([]);
  const [drawing, setDrawing] = useState<Waypoint[] | null>(null);
  const [replay, setReplay] = useState<Flight | null>(null);
  const [replayTime, setReplayTime] = useState(0);
  const [plan, setPlan] = useState<Waypoint[]>([]);
  const [planning, setPlanning] = useState(false);
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

  const showAlerts = useCallback(() => {
    setAlerts(alertStore.current.list());
    setAlerted(alertStore.current.drones());
  }, []);

  // Alerts and the geofence from the alerter: at start and after a reconnect,
  // since both travel on core NATS and any sent meanwhile went by unseen.
  const reloadAlerts = useCallback(async () => {
    try {
      const [a, f] = await Promise.all([fetch("/api/alerts"), fetch("/api/geofence")]);
      if (a.ok) alertStore.current.load((await a.json()) as Alert[]);
      if (f.ok) setFence(((await f.json()) as Fence).polygon ?? []);
      showAlerts();
    } catch {
      // The gateway is down; the banner shows it.
    }
  }, [showAlerts]);

  const reloadMission = useCallback(async (id: number) => {
    try {
      const res = await fetch(`/api/drones/${id}/mission`);
      if (!res.ok) return;
      const body = (await res.json()) as { mission: Mission | null };
      if (selectedRef.current === id) setMission(body.mission);
    } catch {
      // The gateway is down; the banner shows it.
    }
  }, []);

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
          void reloadAlerts();
        }
      },
      alert: (a) => {
        alertStore.current.apply(a);
        showAlerts();
      },
      geofence: (f) => setFence(f.polygon ?? []),
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
        if (u.type === "mission_upload" && u.drone_id === selectedRef.current && u.status !== "pending") {
          void reloadMission(u.drone_id);
        }
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
  }, [reloadLog, reloadFleet, reloadMission, reloadAlerts, showAlerts, showLog]);

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
    setReplay(null);
    setMission(null);
    setPlan([]);
    setPlanning(false);
    if (selected === null) return;
    void reloadMission(selected);
    if (user) void reloadLog(selected);
    const c = conn.current;
    c?.subscribe([selected]);
    return () => c?.unsubscribe([selected]);
  }, [selected, user, reloadLog, reloadMission, showLog]);

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

  const upload = useCallback(
    (altitudeM: number) => {
      const id = selectedRef.current;
      if (id === null) return;
      const requestId = commands.current.start(id, "mission_upload", user);
      if (!conn.current?.upload(requestId, id, plan, altitudeM)) {
        commands.current.notSent(requestId);
      }
      setPlan([]);
      setPlanning(false);
      showLog();
    },
    [user, plan, showLog],
  );

  const drawingRef = useRef(drawing);
  drawingRef.current = drawing;
  const addWaypoint = useCallback((w: Waypoint) => {
    if (drawingRef.current) setDrawing((d) => [...(d ?? []), w]);
    else setPlan((p) => [...p, w]);
  }, []);

  const draw = useCallback((on: boolean) => {
    setDrawing(on ? [] : null);
    if (on) setPlanning(false);
  }, []);

  const saveFence = useCallback(async (polygon: Waypoint[]): Promise<string> => {
    try {
      const res = await fetch("/api/geofence", {
        method: "PUT",
        headers: { "Content-Type": "application/json" },
        body: JSON.stringify({ polygon }),
      });
      const body = (await res.json()) as { fence?: Fence; error?: string };
      if (!res.ok || body.error) return body.error ?? "Could not save the geofence";
      setFence(body.fence?.polygon ?? []);
      setDrawing(null);
      return "";
    } catch {
      return "Could not reach the gateway";
    }
  }, []);
  const clearPlan = useCallback(() => setPlan([]), []);

  const loadReplay = useCallback(async (minutes: number): Promise<string> => {
    const id = selectedRef.current;
    if (id === null) return "";
    const to = new Date();
    const from = new Date(to.getTime() - minutes * 60_000);
    try {
      const res = await fetch(`/api/drones/${id}/history?from=${from.toISOString()}&to=${to.toISOString()}`);
      const body = (await res.json()) as History & { error?: string };
      if (!res.ok) return body.error ?? "Could not load the flight";
      if (selectedRef.current !== id) return "";
      const f = toFlight(body);
      setReplay(f);
      setReplayTime(f.start);
      return "";
    } catch {
      return "Could not reach the gateway";
    }
  }, []);
  const closeReplay = useCallback(() => setReplay(null), []);
  const replayPoint = replay ? pointAt(replay, replayTime) : undefined;
  const replayTrack = useMemo(() => (replay ? track(replay) : []), [replay]);

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
        <AlertList alerts={alerts} onSelect={select} />
        {user && (
          <FleetBar
            live={drones.filter((d) => d.link === "ok").length}
            view={fleet}
            onSend={sendFleet}
            fence={fence}
            drawing={drawing}
            onDraw={draw}
            onSaveFence={saveFence}
          />
        )}
      </DroneList>
      <div className="stage">
        <DroneMap
          drones={drones}
          selected={selected}
          onSelect={select}
          onArea={followArea}
          route={plan.length > 0 || planning ? plan : (mission?.waypoints ?? [])}
          fence={drawing ?? fence}
          planning={planning || drawing !== null}
          onPick={addWaypoint}
          alerted={alerted}
          replayTrack={replayTrack}
          replayAt={replayPoint ? { lat: replayPoint.lat, lon: replayPoint.lon } : null}
        />
        {drone && checked && (
          <CommandPanel
            key={drone.drone_id}
            drone={drone}
            user={user}
            log={log}
            onSend={send}
            onLogin={login}
            onLogout={logout}
            mission={mission}
            plan={plan}
            planning={planning}
            onPlanning={setPlanning}
            onClearPlan={clearPlan}
            onUpload={upload}
            replay={<Replay flight={replay} at={replayTime} onLoad={loadReplay} onSeek={setReplayTime} onClose={closeReplay} />}
          />
        )}
      </div>
    </div>
  );
}
