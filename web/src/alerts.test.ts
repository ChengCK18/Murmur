import { describe, expect, it } from "vitest";
import { Alerts } from "./alerts";
import type { Alert } from "./types";

const alert = (drone: number, kind: Alert["kind"], raised: boolean, at = "2026-11-07T10:00:00Z"): Alert => ({
  drone_id: drone,
  kind,
  raised,
  message: "",
  at,
});

describe("Alerts", () => {
  it("keeps what is raised and drops what is cleared", () => {
    const a = new Alerts();
    a.apply(alert(1, "link_lost", true));
    a.apply(alert(1, "geofence", true));
    a.apply(alert(2, "low_battery", true));
    a.apply(alert(1, "link_lost", false));
    a.apply(alert(3, "link_lost", false)); // never raised here
    expect(a.list().map((x) => `${x.drone_id} ${x.kind}`).sort()).toEqual(["1 geofence", "2 low_battery"]);
    expect([...a.drones()].sort()).toEqual([1, 2]);
  });

  it("lists newest first", () => {
    const a = new Alerts();
    a.apply(alert(1, "link_lost", true, "2026-11-07T10:00:00Z"));
    a.apply(alert(2, "link_lost", true, "2026-11-07T10:00:05Z"));
    expect(a.list().map((x) => x.drone_id)).toEqual([2, 1]);
  });

  it("takes the alerter's list whole after a reconnect", () => {
    const a = new Alerts();
    a.apply(alert(1, "link_lost", true));
    a.load([alert(2, "geofence", true)]);
    expect(a.list().map((x) => x.drone_id)).toEqual([2]);
  });
});
