import { describe, expect, it } from "vitest";
import { Drones, followedArea } from "./drones";
import type { DroneState } from "./types";

function drone(id: number, lastSeen: string, link: "ok" | "lost" = "ok"): DroneState {
  return { tenant_id: 1, drone_id: id, sys_id: id, armed: true, link, last_seen: lastSeen, last_heartbeat: lastSeen };
}

describe("Drones", () => {
  it("drops drones the list no longer has", () => {
    const d = new Drones();
    d.snapshot([drone(1, "2026-11-03T10:00:00Z"), drone(2, "2026-11-03T10:00:00Z")]);
    expect(d.merge([drone(1, "2026-11-03T10:00:01Z")])).toEqual([2]);
    expect(d.list().map((s) => s.drone_id)).toEqual([1]);
  });

  it("keeps the newer state when merging the list", () => {
    const d = new Drones();
    d.update(drone(1, "2026-11-03T10:00:05Z"));
    d.merge([drone(1, "2026-11-03T10:00:01Z")]); // the list is older
    expect(d.list()[0].last_seen).toBe("2026-11-03T10:00:05Z");
    d.merge([drone(1, "2026-11-03T10:00:01Z", "lost")]); // but a link change always shows
    expect(d.list()[0].link).toBe("lost");
  });

  it("removes", () => {
    const d = new Drones();
    d.update(drone(3, "2026-11-03T10:00:00Z"));
    d.remove(3);
    expect(d.list()).toEqual([]);
  });
});

describe("followedArea", () => {
  it("widens the map by a quarter on each side", () => {
    expect(followedArea([10, 20, 14, 28])).toEqual([9, 18, 15, 30]);
  });
});
