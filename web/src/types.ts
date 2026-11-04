// The gateway's drone state (internal/state.Drone in Go).
export interface DroneState {
  tenant_id: number;
  drone_id: number;
  sys_id: number;
  mode?: string;
  armed: boolean;
  link: "ok" | "lost";
  position?: {
    lat: number;
    lon: number;
    alt_m: number;
    rel_alt_m: number;
    speed_ms: number;
    heading_deg: number | null;
  };
  battery?: {
    percent: number | null;
    voltage_v?: number;
    consumed_mah?: number;
  };
  last_seen: string;
  last_heartbeat: string;
}

// A map area in degrees: west, south, east, north.
export type BBox = [number, number, number, number];

// Messages from the gateway (cmd/gateway/hub.go).
export type ServerMessage =
  | { type: "snapshot"; drones: DroneState[] }
  | { type: "update"; drone_id: number; state: DroneState }
  | { type: "remove"; drone_id: number } // silent for the gateway's -stale (10 min)
  | { type: "error"; message: string };
