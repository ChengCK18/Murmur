import { type FormEvent, type ReactNode, useState } from "react";
import { isFinal, STATUS_LABELS, TYPE_LABELS } from "./commands";
import { altitudeLabel, batteryLabel } from "./drones";
import type { CommandParams, CommandType, CommandUpdate, DroneState, Mission, Waypoint } from "./types";

const MODES = ["GUIDED", "AUTO", "RTL", "LAND"];

interface Props {
  drone: DroneState;
  user: string;
  log: CommandUpdate[];
  onSend(type: CommandType, params?: CommandParams): void;
  onLogin(username: string, password: string): Promise<string>;
  onLogout(): void;
  mission: Mission | null; // what the drone holds
  plan: Waypoint[]; // the mission being drawn
  planning: boolean;
  onPlanning(on: boolean): void;
  onClearPlan(): void;
  onUpload(altitudeM: number): void;
  replay: ReactNode; // shown with or without a login
}

// A command that waits for a second click: arm and takeoff (plan §4).
interface Pending {
  type: CommandType;
  params?: CommandParams;
  question: string;
}

// CommandPanel sends commands to the selected drone and shows how each ended.
export function CommandPanel({ drone, user, log, onSend, onLogin, onLogout, replay, ...mission }: Props) {
  return (
    <section className="panel" aria-label={`Drone ${drone.drone_id}`}>
      <h2>Drone {drone.drone_id}</h2>
      <p className="state">
        {drone.mode ?? "?"} · {drone.armed ? "armed" : "disarmed"} · {altitudeLabel(drone)} · {batteryLabel(drone)} ·{" "}
        {drone.link === "ok" ? "link OK" : "link lost"}
      </p>
      {user ? (
        <>
          <div className="who">
            <span>Logged in as {user}</span>
            <button className="link" onClick={onLogout}>
              Log out
            </button>
          </div>
          <Controls drone={drone} onSend={onSend} />
          <MissionControls {...mission} onSend={onSend} droneId={drone.drone_id} />
          <Log log={log} />
        </>
      ) : (
        <Login onLogin={onLogin} />
      )}
      {replay}
    </section>
  );
}

function Login({ onLogin }: { onLogin: Props["onLogin"] }) {
  const [username, setUsername] = useState("demo");
  const [password, setPassword] = useState("");
  const [error, setError] = useState("");

  const submit = async (e: FormEvent) => {
    e.preventDefault();
    setError(await onLogin(username, password));
  };

  return (
    <form onSubmit={submit}>
      <h3>Log in to send commands</h3>
      <div className="row">
        <input aria-label="Username" value={username} onChange={(e) => setUsername(e.target.value)} autoComplete="username" />
      </div>
      <div className="row">
        <input
          aria-label="Password"
          type="password"
          placeholder="Password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          autoComplete="current-password"
        />
      </div>
      <div className="row">
        <button type="submit">Log in</button>
      </div>
      {error && <p className="error">{error}</p>}
    </form>
  );
}

function Controls({ drone, onSend }: { drone: DroneState; onSend: Props["onSend"] }) {
  const [altitude, setAltitude] = useState(20);
  const [mode, setMode] = useState("GUIDED");
  const [confirming, setConfirming] = useState<Pending | null>(null);

  const id = drone.drone_id;
  return (
    <>
      <div className="row">
        <button onClick={() => setConfirming({ type: "arm", question: `Arm drone ${id}? Its motors will start.` })}>Arm</button>
        <button onClick={() => onSend("disarm")}>Disarm</button>
      </div>
      <div className="row">
        <select aria-label="Mode" value={mode} onChange={(e) => setMode(e.target.value)}>
          {MODES.map((m) => (
            <option key={m}>{m}</option>
          ))}
        </select>
        <button onClick={() => onSend("set_mode", { mode })}>Set mode</button>
      </div>
      <div className="row">
        <input
          aria-label="Takeoff altitude in metres"
          type="number"
          min={1}
          max={120}
          value={altitude}
          onChange={(e) => setAltitude(Number(e.target.value))}
        />
        <button
          disabled={altitude <= 0}
          onClick={() =>
            setConfirming({ type: "takeoff", params: { altitude_m: altitude }, question: `Take off drone ${id} to ${altitude} m?` })
          }
        >
          Takeoff
        </button>
      </div>
      <div className="row">
        <button onClick={() => onSend("return_to_launch")}>Return to launch</button>
        <button onClick={() => onSend("land")}>Land</button>
      </div>
      {confirming && (
        <div className="confirm" role="alertdialog" aria-label="Confirm">
          <p>{confirming.question}</p>
          <div className="row">
            <button
              className="yes"
              onClick={() => {
                onSend(confirming.type, confirming.params);
                setConfirming(null);
              }}
            >
              Confirm
            </button>
            <button onClick={() => setConfirming(null)}>Cancel</button>
          </div>
        </div>
      )}
    </>
  );
}

type MissionProps = Pick<Props, "mission" | "plan" | "planning" | "onPlanning" | "onClearPlan" | "onUpload" | "onSend"> & {
  droneId: number;
};

function MissionControls({ mission, plan, planning, onPlanning, onClearPlan, onUpload, onSend, droneId }: MissionProps) {
  const [altitude, setAltitude] = useState(20);
  const [confirming, setConfirming] = useState(false);
  const ready = mission?.status === "uploaded";

  return (
    <>
      <h3>Mission</h3>
      {planning ? (
        <p className="hint">Click the map to add waypoints. {plan.length} so far.</p>
      ) : (
        <p className="hint" data-testid="mission-state">
          {mission ? `${mission.waypoints.length} waypoints at ${mission.altitude_m} m: ${mission.status}` : "No mission uploaded"}
        </p>
      )}
      <div className="row">
        <button onClick={() => onPlanning(!planning)}>{planning ? "Stop adding" : "Plan on map"}</button>
        <button disabled={plan.length === 0} onClick={onClearPlan}>
          Clear
        </button>
      </div>
      <div className="row">
        <input
          aria-label="Mission altitude in metres"
          type="number"
          min={1}
          max={120}
          value={altitude}
          onChange={(e) => setAltitude(Number(e.target.value))}
        />
        <button disabled={plan.length === 0 || altitude <= 0} onClick={() => onUpload(altitude)}>
          Upload
        </button>
      </div>
      <div className="row">
        <button disabled={!ready} onClick={() => setConfirming(true)}>
          Start mission
        </button>
      </div>
      {confirming && (
        <div className="confirm" role="alertdialog" aria-label="Confirm">
          <p>Start the mission on drone {droneId}?</p>
          <div className="row">
            <button
              className="yes"
              onClick={() => {
                onSend("mission_start");
                setConfirming(false);
              }}
            >
              Confirm
            </button>
            <button onClick={() => setConfirming(false)}>Cancel</button>
          </div>
        </div>
      )}
    </>
  );
}

function badge(u: CommandUpdate): string {
  if (u.status === "accepted") return "badge good";
  return isFinal(u.status) ? "badge bad" : "badge";
}

function Log({ log }: { log: CommandUpdate[] }) {
  if (log.length === 0) return null;
  return (
    <>
      <h3>Commands</h3>
      <ul className="log" aria-label="Commands">
        {log.slice(0, 10).map((u, i) => (
          <li key={u.command_id ?? `local-${i}`} data-status={u.status}>
            <span className="line">
              <span>{TYPE_LABELS[u.type] ?? u.type}</span>
              <span className={badge(u)}>{STATUS_LABELS[u.status]}</span>
            </span>
            {u.reason && <span className="reason">{u.reason}</span>}
          </li>
        ))}
      </ul>
    </>
  );
}
