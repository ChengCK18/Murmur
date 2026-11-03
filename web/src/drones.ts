import type { DroneState } from "./types";

// Drones holds the latest state of every drone the page knows. States from
// the REST list and the WebSocket meet here; the newer one (by last_seen) wins,
// except that a snapshot always replaces: it is the gateway's own view after
// a (re)connect.
export class Drones {
  private readonly byId = new Map<number, DroneState>();

  snapshot(states: DroneState[]): void {
    for (const s of states) this.byId.set(s.drone_id, s);
  }

  update(s: DroneState): void {
    this.byId.set(s.drone_id, s);
  }

  remove(id: number): void {
    this.byId.delete(id);
  }

  // merge takes the periodic REST list: it keeps whichever state is newer,
  // and drops drones the list no longer has (the gateway hides drones silent
  // for 10 minutes). It returns the dropped IDs.
  merge(states: DroneState[]): number[] {
    const listed = new Set(states.map((s) => s.drone_id));
    const dropped = [...this.byId.keys()].filter((id) => !listed.has(id));
    dropped.forEach((id) => this.byId.delete(id));
    for (const s of states) {
      const cur = this.byId.get(s.drone_id);
      if (!cur || Date.parse(s.last_seen) >= Date.parse(cur.last_seen) || s.link !== cur.link) {
        this.byId.set(s.drone_id, s);
      }
    }
    return dropped;
  }

  list(): DroneState[] {
    return [...this.byId.values()].sort((a, b) => a.drone_id - b.drone_id);
  }
}

// Labels shown on the map and in the list.
export function altitudeLabel(s: DroneState): string {
  return s.position ? `${s.position.rel_alt_m.toFixed(1)} m` : "–";
}

export function batteryLabel(s: DroneState): string {
  const p = s.battery?.percent;
  return p == null ? "–" : `${p}%`;
}
