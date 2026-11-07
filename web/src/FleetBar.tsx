import { useState } from "react";
import { type FleetView, STATUS_LABELS, TYPE_LABELS } from "./commands";
import type { CommandStatus, CommandType } from "./types";

interface Props {
  live: number; // drones with a link
  view: FleetView | null;
  onSend(type: CommandType): void;
}

// FleetBar sends one command to every live drone and adds up the results
// (plan §4, "Fleet commands"). Each drone is tracked on its own, so the totals
// come straight from their statuses.
export function FleetBar({ live, view, onSend }: Props) {
  const [confirming, setConfirming] = useState<CommandType | null>(null);
  const label = confirming === "land" ? "Land" : "Return to launch";

  return (
    <div className="fleet">
      <div className="row">
        <button disabled={live === 0} onClick={() => setConfirming("return_to_launch")}>
          Return all to launch
        </button>
        <button disabled={live === 0} onClick={() => setConfirming("land")}>
          Land all
        </button>
      </div>
      {confirming && (
        <div className="confirm" role="alertdialog" aria-label="Confirm fleet command">
          <p>
            {label}: all {live} live drones?
          </p>
          <div className="row">
            <button
              className="yes"
              onClick={() => {
                onSend(confirming);
                setConfirming(null);
              }}
            >
              Confirm
            </button>
            <button onClick={() => setConfirming(null)}>Cancel</button>
          </div>
        </div>
      )}
      {view && <Summary view={view} />}
    </div>
  );
}

function Summary({ view }: { view: FleetView }) {
  const name = TYPE_LABELS[view.type] ?? view.type;
  if (view.error) {
    return <p className="error">{`${name}: ${view.error}`}</p>;
  }
  const parts = (Object.entries(view.totals) as [CommandStatus, number][]).map(
    ([status, n]) => `${n} ${STATUS_LABELS[status].toLowerCase()}`,
  );
  if (view.waiting > 0) parts.push(`${view.waiting} waiting`);
  return (
    <>
      <p className="summary" data-testid="fleet-summary" data-waiting={view.waiting}>
        {name}: {parts.join(", ")}
      </p>
      {view.failures.length > 0 && (
        <details>
          <summary>Show {view.failures.length} not accepted</summary>
          <ul>
            {view.failures.map((u) => (
              <li key={u.drone_id}>
                Drone {u.drone_id}: {STATUS_LABELS[u.status].toLowerCase()}
                {u.reason ? `, ${u.reason}` : ""}
              </li>
            ))}
          </ul>
        </details>
      )}
    </>
  );
}
