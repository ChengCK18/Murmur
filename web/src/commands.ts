import type { CommandStatus, CommandUpdate } from "./types";

const FINAL: ReadonlySet<CommandStatus> = new Set([
  "accepted",
  "rejected",
  "failed",
  "timed_out",
  "expired",
  "unreachable",
  "unknown",
]);

export const isFinal = (s: CommandStatus): boolean => FINAL.has(s);

// What the page calls each status.
export const STATUS_LABELS: Record<CommandStatus, string> = {
  pending: "Pending",
  sent: "Sent",
  accepted: "Accepted",
  rejected: "Rejected",
  failed: "Failed",
  timed_out: "Timed out",
  expired: "Expired",
  unreachable: "Unreachable",
  unknown: "Unknown",
};

// What the page calls each command.
export const TYPE_LABELS: Record<string, string> = {
  arm: "Arm",
  disarm: "Disarm",
  set_mode: "Set mode",
  takeoff: "Takeoff",
  return_to_launch: "Return to launch",
  land: "Land",
  mission_start: "Start mission",
  mission_upload: "Mission upload",
};

const LOST = "The connection was lost before an answer. Check the drone's state.";

// FleetView is one fleet command as the page shows it.
export interface FleetView {
  type: string;
  count: number; // drones it went to
  fleetId?: string; // unknown until the command service answers
  error?: string; // refused whole
  totals: Partial<Record<CommandStatus, number>>;
  waiting: number; // no final status yet
  failures: CommandUpdate[]; // final, and not accepted
}

// Commands holds what the page knows of each command, from three sources that
// can arrive in any order: the page's own request, statuses pushed by the
// gateway, and the list reloaded after a reconnect. A status never goes
// backwards, and a final one is never replaced.
export class Commands {
  private next = 1;
  private readonly local = new Map<string, CommandUpdate>(); // waiting for the first answer, by request ID
  private readonly byId = new Map<string, CommandUpdate>();
  private readonly order: (string | CommandUpdate)[] = []; // newest last

  // start records a command about to be sent, and returns its request ID.
  start(droneId: number, type: string, user: string): string {
    const requestId = `r${this.next++}`;
    const u: CommandUpdate = { drone_id: droneId, type, status: "pending", attempts: 0, issued_by: user, at: new Date().toISOString() };
    this.local.set(requestId, u);
    this.order.push(u);
    return requestId;
  }

  // notSent ends a command the socket would not take.
  notSent(requestId: string): void {
    const u = this.local.get(requestId);
    if (!u) return;
    u.status = "rejected";
    u.reason = "Not connected to the gateway";
    this.local.delete(requestId);
  }

  // apply takes a status from the gateway. requestId is set on the answer to
  // this page's own request.
  apply(update: CommandUpdate, requestId?: string): void {
    const mine = requestId ? this.local.get(requestId) : undefined;
    if (mine) {
      this.local.delete(requestId!);
      if (!update.command_id) {
        Object.assign(mine, update); // refused before it had an ID
        return;
      }
      const seen = this.byId.get(update.command_id);
      if (seen) {
        // A pushed status got here first: that entry stands for this command.
        this.order.splice(this.order.indexOf(mine), 1);
        this.merge(seen, update);
        return;
      }
      Object.assign(mine, update);
      this.byId.set(update.command_id, mine);
      return;
    }
    if (!update.command_id) {
      // Refused before it had an ID. One of a fleet's still counts towards the
      // totals; otherwise it is the answer to a request of another page.
      if (update.fleet_id) this.order.push({ ...update });
      return;
    }
    const seen = this.byId.get(update.command_id);
    if (seen) {
      this.merge(seen, update);
      return;
    }
    const u = { ...update };
    this.byId.set(update.command_id, u);
    this.order.push(u);
  }

  private merge(into: CommandUpdate, update: CommandUpdate): void {
    if (isFinal(into.status)) return;
    if (update.status === "pending" && into.status !== "pending") return;
    Object.assign(into, update);
  }

  // load takes a drone's latest commands from the command service.
  load(updates: CommandUpdate[]): void {
    for (const u of [...updates].reverse()) this.apply(u);
  }

  // connectionLost ends commands that never got a first answer. Any that
  // reached the command service come back through load, as themselves.
  connectionLost(): void {
    for (const [requestId, u] of this.local) {
      u.status = "unknown";
      u.reason = LOST;
      this.local.delete(requestId);
    }
  }

  clear(): void {
    this.local.clear();
    this.byId.clear();
    this.order.length = 0;
  }

  // fleet sums up a fleet command from the statuses seen so far.
  fleet(type: string, count: number, fleetId?: string, error?: string): FleetView {
    const updates = fleetId ? (this.order as CommandUpdate[]).filter((u) => u.fleet_id === fleetId) : [];
    const totals: FleetView["totals"] = {};
    const failures: CommandUpdate[] = [];
    let ended = 0;
    for (const u of updates) {
      if (!isFinal(u.status)) continue;
      ended++;
      totals[u.status] = (totals[u.status] ?? 0) + 1;
      if (u.status !== "accepted") failures.push({ ...u });
    }
    failures.sort((a, b) => a.drone_id - b.drone_id);
    return { type, count, fleetId, error, totals, failures, waiting: error ? 0 : Math.max(count - ended, 0) };
  }

  // forDrone returns a drone's commands, newest first.
  forDrone(droneId: number): CommandUpdate[] {
    return (this.order as CommandUpdate[])
      .filter((u) => u.drone_id === droneId)
      .reverse()
      .map((u) => ({ ...u }));
  }
}
