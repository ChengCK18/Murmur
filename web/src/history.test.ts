import { describe, expect, it } from "vitest";
import { flight, type History, pointAt, track } from "./history";

const h: History = {
  drone_id: 7,
  from: "2026-11-07T10:00:00Z",
  to: "2026-11-07T10:01:00Z",
  step_ms: 1000,
  points: [0, 1, 2, 5].map((s) => ({
    t: `2026-11-07T10:00:0${s}Z`,
    lat: -35 + s,
    lon: 149,
    alt_m: 600,
    rel_alt_m: s,
    speed_ms: 0,
    heading_deg: null,
    battery_pct: 90,
    armed: true,
  })),
};

describe("replay", () => {
  const f = flight(h);
  const at = (s: number) => pointAt(f, Date.parse("2026-11-07T10:00:00Z") + s * 1000)?.rel_alt_m;

  it("finds where the drone was at a moment", () => {
    expect([at(0), at(0.5), at(1), at(4.9), at(5), at(30)]).toEqual([0, 0, 1, 2, 5, 5]);
  });

  it("starts at the first point before the flight", () => {
    expect(at(-10)).toBe(0);
  });

  it("knows its span and track", () => {
    expect(f.end - f.start).toBe(5000);
    expect(track(f)).toHaveLength(4);
  });

  it("copes with a range that held nothing", () => {
    const empty = flight({ ...h, points: [] });
    expect(pointAt(empty, empty.start)).toBeUndefined();
    expect(empty.end - empty.start).toBe(60_000);
  });
});
