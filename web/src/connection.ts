import type { BBox, DroneState, ServerMessage } from "./types";

export type Status = "connecting" | "open" | "reconnecting";

export interface Handlers {
  snapshot(drones: DroneState[]): void;
  update(state: DroneState): void;
  remove(id: number): void;
  status(status: Status): void;
  error?(message: string): void;
}

// The parts of a WebSocket the connection uses, so tests can fake one.
export interface SocketLike {
  readyState: number;
  send(data: string): void;
  close(): void;
  onopen: (() => void) | null;
  onmessage: ((ev: { data: unknown }) => void) | null;
  onclose: (() => void) | null;
}

const OPEN = 1;

// Delay before reconnect attempt n (0-based): 1, 2, 4, 8, then 10 s (guide step 3.6).
export function backoff(attempt: number): number {
  return Math.min(1000 * 2 ** attempt, 10_000);
}

// Connection keeps a WebSocket to the gateway open. After the socket closes it
// retries with backoff, then asks again for its area and every drone so the
// gateway sends a fresh snapshot.
export class Connection {
  private socket?: SocketLike;
  private attempt = 0;
  private timer?: ReturnType<typeof setTimeout>;
  private stopped = false;
  private readonly subs = new Set<number>();
  private bbox?: BBox;

  constructor(
    private readonly url: string,
    private readonly handlers: Handlers,
    private readonly open: (url: string) => SocketLike = (u) => new WebSocket(u) as unknown as SocketLike,
  ) {}

  start(): void {
    this.handlers.status("connecting");
    this.connect();
  }

  stop(): void {
    this.stopped = true;
    clearTimeout(this.timer);
    this.socket?.close();
  }

  // subscribe adds drones; only new ones are sent to the gateway.
  subscribe(ids: number[]): void {
    const fresh = ids.filter((id) => !this.subs.has(id));
    fresh.forEach((id) => this.subs.add(id));
    if (fresh.length > 0 && this.socket?.readyState === OPEN) {
      this.socket.send(JSON.stringify({ type: "subscribe", drones: fresh }));
    }
  }

  // unsubscribe drops drones, freeing their place under the gateway's limit.
  unsubscribe(ids: number[]): void {
    const gone = ids.filter((id) => this.subs.delete(id));
    if (gone.length > 0 && this.socket?.readyState === OPEN) {
      this.socket.send(JSON.stringify({ type: "unsubscribe", drones: gone }));
    }
  }

  // area replaces the map area followed: the gateway sends the drones in it.
  area(bbox: BBox): void {
    this.bbox = bbox;
    if (this.socket?.readyState === OPEN) {
      this.socket.send(JSON.stringify({ type: "subscribe_area", bbox }));
    }
  }

  private connect(): void {
    const s = this.open(this.url);
    this.socket = s;
    s.onopen = () => {
      this.attempt = 0;
      this.handlers.status("open");
      if (this.bbox) {
        s.send(JSON.stringify({ type: "subscribe_area", bbox: this.bbox }));
      }
      if (this.subs.size > 0) {
        s.send(JSON.stringify({ type: "subscribe", drones: [...this.subs] }));
      }
    };
    s.onmessage = (ev) => this.dispatch(ev.data);
    s.onclose = () => {
      if (this.stopped || s !== this.socket) return;
      this.handlers.status("reconnecting");
      this.timer = setTimeout(() => this.connect(), backoff(this.attempt++));
    };
  }

  private dispatch(data: unknown): void {
    let msg: ServerMessage;
    try {
      msg = JSON.parse(String(data)) as ServerMessage;
    } catch {
      return;
    }
    switch (msg.type) {
      case "snapshot":
        this.handlers.snapshot(msg.drones);
        break;
      case "update":
        this.handlers.update(msg.state);
        break;
      case "remove":
        this.handlers.remove(msg.drone_id);
        break;
      case "error":
        this.handlers.error?.(msg.message);
        break;
    }
  }
}

// wsURL is the gateway's WebSocket on the page's own host (through the Vite
// proxy in development).
export function wsURL(loc: Location = window.location): string {
  return `${loc.protocol === "https:" ? "wss" : "ws"}://${loc.host}/ws`;
}
