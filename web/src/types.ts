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

// A command's status (internal/command.Update in Go).
export type CommandStatus =
  | "pending"
  | "sent"
  | "accepted"
  | "rejected"
  | "failed"
  | "timed_out"
  | "expired"
  | "unreachable"
  | "unknown";

export interface CommandUpdate {
  command_id?: string; // missing if it was refused before it got one
  fleet_id?: string;
  drone_id: number;
  type: string;
  status: CommandStatus;
  reason?: string;
  attempts: number;
  mav_result?: number;
  issued_by: string;
  at: string;
}

export type CommandType = "arm" | "disarm" | "set_mode" | "takeoff" | "return_to_launch" | "land" | "mission_start";

export interface CommandParams {
  altitude_m?: number;
  mode?: string;
}

export interface Waypoint {
  lat: number;
  lon: number;
}

// The mission a drone holds (internal/command.MissionView in Go).
export interface Mission {
  waypoints: Waypoint[];
  altitude_m: number;
  status: "uploading" | "uploaded" | "failed";
}

// An alert from the alerter (internal/alert.Alert in Go).
export interface Alert {
  drone_id: number;
  kind: "low_battery" | "link_lost" | "geofence";
  raised: boolean;
  message: string;
  at: string;
}

export interface Fence {
  polygon: Waypoint[];
  updated_by?: string;
}

// A map area in degrees: west, south, east, north.
export type BBox = [number, number, number, number];

// Messages from the gateway (cmd/gateway/hub.go).
export type ServerMessage =
  | { type: "snapshot"; drones: DroneState[] }
  | { type: "update"; drone_id: number; state: DroneState }
  | { type: "remove"; drone_id: number } // silent for the gateway's -stale (10 min)
  | { type: "command_status"; request_id?: string; update: CommandUpdate }
  | { type: "fleet_started"; request_id?: string; fleet_id?: string; count: number; error?: string }
  | { type: "alert"; alert: Alert }
  | { type: "geofence"; fence: Fence }
  | { type: "error"; message: string };
