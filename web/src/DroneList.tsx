import type { ReactNode } from "react";
import type { DroneState } from "./types";
import { altitudeLabel, batteryLabel } from "./drones";

interface Props {
  drones: DroneState[];
  selected: number | null;
  onSelect(id: number): void;
  children?: ReactNode;
}

export function DroneList({ drones, selected, onSelect, children }: Props) {
  return (
    <aside className="list">
      <div className="brand">
        <svg viewBox="-10 -10 20 20" width="18" height="18" aria-hidden="true">
          <path d="M0,-9 L7,8 L0,4 L-7,8 Z" />
        </svg>
        Murmur
      </div>
      {children}
      <h1>Drones ({drones.length})</h1>
      {drones.length === 0 && <p className="empty">No drones yet.</p>}
      <ul>
        {drones.map((d) => (
          <li
            key={d.drone_id}
            className={[d.link === "lost" && "lost", d.drone_id === selected && "selected"].filter(Boolean).join(" ")}
            onClick={() => onSelect(d.drone_id)}
            data-drone-id={d.drone_id}
          >
            <span className="id">{d.drone_id}</span>
            <span className="mode">
              {d.mode ?? "?"} {d.armed ? "armed" : "disarmed"}
            </span>
            <span className="num">{altitudeLabel(d)}</span>
            <span className="num">{batteryLabel(d)}</span>
            <span className="link">{d.link === "ok" ? "link OK" : "link lost"}</span>
          </li>
        ))}
      </ul>
    </aside>
  );
}
