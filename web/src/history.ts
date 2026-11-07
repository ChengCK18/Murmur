import type { Waypoint } from "./types";

// HistoryPoint is one moment of a stored flight (cmd/gateway/history.go).
export interface HistoryPoint {
  t: string;
  lat: number;
  lon: number;
  alt_m: number;
  rel_alt_m: number;
  speed_ms: number;
  heading_deg: number | null;
  battery_pct: number | null;
  mode?: string;
  armed: boolean;
}

export interface History {
  drone_id: number;
  from: string;
  to: string;
  step_ms: number;
  points: HistoryPoint[];
}

// Flight is a loaded history with its times parsed once.
export interface Flight {
  droneId: number;
  points: HistoryPoint[];
  times: number[]; // ms since 1970, one per point
  start: number;
  end: number;
}

export function flight(h: History): Flight {
  const times = h.points.map((p) => Date.parse(p.t));
  return {
    droneId: h.drone_id,
    points: h.points,
    times,
    start: times[0] ?? Date.parse(h.from),
    end: times.at(-1) ?? Date.parse(h.to),
  };
}

// pointAt returns the last point at or before t: where the drone was then.
export function pointAt(f: Flight, t: number): HistoryPoint | undefined {
  let lo = 0;
  let hi = f.times.length - 1;
  if (hi < 0 || t < f.times[0]) return f.points[0];
  while (lo < hi) {
    const mid = (lo + hi + 1) >> 1;
    if (f.times[mid] <= t) lo = mid;
    else hi = mid - 1;
  }
  return f.points[lo];
}

export function track(f: Flight): Waypoint[] {
  return f.points.map((p) => ({ lat: p.lat, lon: p.lon }));
}
