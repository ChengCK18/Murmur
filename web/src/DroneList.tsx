import type { DroneState } from "./types";
import { altitudeLabel, batteryLabel } from "./drones";

interface Props {
  drones: DroneState[];
  selected: number | null;
  onSelect(id: number): void;
}

export function DroneList({ drones, selected, onSelect }: Props) {
  return (
    <aside className="list">
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
