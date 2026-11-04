import { useCallback, useEffect, useRef, useState } from "react";
import { Connection, type Status, wsURL } from "./connection";
import { Drones } from "./drones";
import { DroneList } from "./DroneList";
import { DroneMap } from "./DroneMap";
import type { BBox, DroneState } from "./types";

const LIST_EVERY = 10_000; // ms between refreshes of the whole fleet's list

export function App() {
  const [drones, setDrones] = useState<DroneState[]>([]);
  const [status, setStatus] = useState<Status>("connecting");
  const [selected, setSelected] = useState<number | null>(null);
  const store = useRef(new Drones());
  const conn = useRef<Connection | null>(null);
  const area = useRef<BBox | null>(null);

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
      status: setStatus,
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
  }, []);

  // The selected drone stays live wherever the map is.
  useEffect(() => {
    if (selected === null) return;
    const c = conn.current;
    c?.subscribe([selected]);
    return () => c?.unsubscribe([selected]);
  }, [selected]);

  const select = useCallback((id: number) => setSelected(id), []);
  const followArea = useCallback((b: BBox) => {
    area.current = b;
    conn.current?.area(b);
  }, []);

  return (
    <div className="app">
      {status !== "open" && (
        <div className="banner" data-testid="banner">
          {status === "connecting" ? "Connecting…" : "Connection lost, reconnecting…"}
        </div>
      )}
      <DroneList drones={drones} selected={selected} onSelect={select} />
      <DroneMap drones={drones} selected={selected} onSelect={select} onArea={followArea} />
    </div>
  );
}
