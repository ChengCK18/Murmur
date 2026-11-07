import { ALERT_LABELS } from "./alerts";
import type { Alert } from "./types";

interface Props {
  alerts: Alert[];
  onSelect(id: number): void;
}

// AlertList shows the alerts raised now, newest first.
export function AlertList({ alerts, onSelect }: Props) {
  if (alerts.length === 0) return null;
  return (
    <section className="alerts" aria-label="Alerts">
      <h2>Alerts ({alerts.length})</h2>
      <ul>
        {alerts.slice(0, 50).map((a) => (
          <li key={`${a.drone_id}:${a.kind}`} data-kind={a.kind} onClick={() => onSelect(a.drone_id)}>
            <span className="what">
              Drone {a.drone_id}: {ALERT_LABELS[a.kind]}
            </span>
            <span className="when">{new Date(a.at).toLocaleTimeString()}</span>
            <span className="msg">{a.message}</span>
          </li>
        ))}
      </ul>
    </section>
  );
}
