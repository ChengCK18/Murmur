import type { Alert } from "./types";

export const ALERT_LABELS: Record<Alert["kind"], string> = {
  low_battery: "Low battery",
  link_lost: "Link lost",
  geofence: "Geofence breach",
};

// Alerts holds the alerts raised now, at most one of each kind per drone. A
// cleared alert removes its raised one; a cleared one never seen is ignored.
export class Alerts {
  private readonly byKey = new Map<string, Alert>();

  apply(a: Alert): void {
    const key = `${a.drone_id}:${a.kind}`;
    if (a.raised) this.byKey.set(key, a);
    else this.byKey.delete(key);
  }

  // load replaces everything with the alerter's own list, after a reconnect.
  load(active: Alert[]): void {
    this.byKey.clear();
    active.forEach((a) => this.apply(a));
  }

  // list returns the alerts raised now, newest first.
  list(): Alert[] {
    return [...this.byKey.values()].sort((a, b) => b.at.localeCompare(a.at) || a.drone_id - b.drone_id);
  }

  // drones returns the IDs of drones with any alert.
  drones(): Set<number> {
    return new Set([...this.byKey.values()].map((a) => a.drone_id));
  }
}
