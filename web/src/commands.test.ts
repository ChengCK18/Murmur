import { describe, expect, it } from "vitest";
import { Commands, isFinal } from "./commands";
import type { CommandUpdate } from "./types";

function status(s: CommandUpdate["status"], id = "c1", extra: Partial<CommandUpdate> = {}): CommandUpdate {
  return { command_id: id, drone_id: 1, type: "arm", status: s, attempts: 1, issued_by: "demo", at: "", ...extra };
}

describe("Commands", () => {
  it("follows a command from the click to its result", () => {
    const c = new Commands();
    const r = c.start(1, "arm", "demo");
    expect(c.forDrone(1).map((u) => u.status)).toEqual(["pending"]);
    c.apply(status("pending"), r);
    c.apply(status("sent"));
    c.apply(status("accepted"));
    expect(c.forDrone(1).map((u) => u.status)).toEqual(["accepted"]);
  });

  it("keeps one entry when a pushed status beats the answer to the request", () => {
    const c = new Commands();
    const r = c.start(1, "arm", "demo");
    c.apply(status("sent"));
    c.apply(status("accepted"));
    c.apply(status("pending"), r);
    expect(c.forDrone(1).map((u) => u.status)).toEqual(["accepted"]);
  });

  it("never lets a status go backwards", () => {
    const c = new Commands();
    c.apply(status("sent"));
    c.apply(status("pending"));
    expect(c.forDrone(1)[0].status).toBe("sent");
    c.apply(status("timed_out"));
    c.apply(status("sent"));
    expect(c.forDrone(1)[0].status).toBe("timed_out");
  });

  it("shows a command refused before it had an ID", () => {
    const c = new Commands();
    const r = c.start(1, "takeoff", "demo");
    c.apply({ drone_id: 1, type: "takeoff", status: "rejected", reason: "log in to send commands", attempts: 0, issued_by: "", at: "" }, r);
    expect(c.forDrone(1)[0]).toMatchObject({ status: "rejected", reason: "log in to send commands" });
  });

  it("ends a command the socket would not take", () => {
    const c = new Commands();
    c.notSent(c.start(1, "arm", "demo"));
    expect(c.forDrone(1)[0].status).toBe("rejected");
  });

  it("leaves no command waiting when the connection drops", () => {
    const c = new Commands();
    c.start(1, "arm", "demo");
    c.connectionLost();
    const [u] = c.forDrone(1);
    expect(isFinal(u.status)).toBe(true);
    expect(u.reason).toMatch(/Check the drone/);
  });

  it("merges the list reloaded after a reconnect", () => {
    const c = new Commands();
    c.apply(status("sent", "c2"));
    // Newest first, as the command service sends them.
    c.load([status("accepted", "c2"), status("rejected", "c1", { type: "land" })]);
    expect(c.forDrone(1).map((u) => [u.command_id, u.status])).toEqual([
      ["c1", "rejected"],
      ["c2", "accepted"],
    ]);
    c.load([status("accepted", "c2")]);
    expect(c.forDrone(1)).toHaveLength(2);
  });

  it("lists newest first, one drone at a time", () => {
    const c = new Commands();
    c.apply(status("accepted", "a"));
    c.apply(status("accepted", "b", { drone_id: 2 }));
    c.apply(status("accepted", "d"));
    expect(c.forDrone(1).map((u) => u.command_id)).toEqual(["d", "a"]);
    expect(c.forDrone(2).map((u) => u.command_id)).toEqual(["b"]);
  });

  it("forgets everything at logout", () => {
    const c = new Commands();
    c.apply(status("accepted"));
    c.clear();
    expect(c.forDrone(1)).toEqual([]);
  });
});
