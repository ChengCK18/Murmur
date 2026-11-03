import { useCallback, useEffect, useRef, useState } from "react";
import { Connection, type Status, wsURL } from "./connection";
import { Drones } from "./drones";
import { DroneList } from "./DroneList";
import { DroneMap } from "./DroneMap";
import type { DroneState } from "./types";

const MAX_SUBSCRIPTIONS = 100; // the gateway's limit (plan open item 4)
const LIST_EVERY = 10_000; // ms between drone list refreshes, to find new drones

export function App() {
  const [drones, setDrones] = useState<DroneState[]>([]);
  const [status, setStatus] = useState<Status>("connecting");
  const [selected, setSelected] = useState<number | null>(null);
  const store = useRef(new Drones());

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

    const conn = new Connection(wsURL(), {
      status: setStatus,
      snapshot: (ds) => {
        store.current.snapshot(ds);
        render();
      },
      update: (d) => {
        store.current.update(d);
        render();
      },
      error: (msg) => console.warn("gateway:", msg),
    });

    // The REST list finds drones (new ones too) to subscribe to.
    const refresh = async () => {
      try {
        const res = await fetch("/api/drones");
        if (!res.ok) return;
        const body = (await res.json()) as { drones: DroneState[] };
        store.current.merge(body.drones);
        conn.subscribe(body.drones.slice(0, MAX_SUBSCRIPTIONS).map((d) => d.drone_id));
        render();
      } catch {
        // The gateway is down; the banner shows it and the next refresh retries.
      }
    };

    conn.start();
    refresh();
    const timer = setInterval(refresh, LIST_EVERY);
    return () => {
      clearInterval(timer);
      cancelAnimationFrame(frame);
      conn.stop();
    };
  }, []);

  const select = useCallback((id: number) => setSelected(id), []);

  return (
    <div className="app">
      {status !== "open" && (
        <div className="banner" data-testid="banner">
          {status === "connecting" ? "Connecting…" : "Connection lost, reconnecting…"}
        </div>
      )}
      <DroneList drones={drones} selected={selected} onSelect={select} />
      <DroneMap drones={drones} selected={selected} onSelect={select} />
    </div>
  );
}
