import { useEffect, useState } from "react";
import { type Flight, pointAt } from "./history";

const RANGES: [string, number][] = [
  ["Last 10 minutes", 10],
  ["Last hour", 60],
  ["Last 6 hours", 360],
];
const SPEEDS = [1, 10, 60];
const FRAME = 100; // ms between replay frames

interface Props {
  flight: Flight | null;
  at: number; // the moment shown, ms since 1970
  onLoad(minutes: number): Promise<string>;
  onSeek(t: number): void;
  onClose(): void;
}

// Replay plays a drone's stored flight back with a time scrubber (guide step
// 6.1). The live marker carries on; the replay is drawn beside it.
export function Replay({ flight, at, onLoad, onSeek, onClose }: Props) {
  const [minutes, setMinutes] = useState(10);
  const [error, setError] = useState("");
  const [loading, setLoading] = useState(false);
  const [playing, setPlaying] = useState(false);
  const [speed, setSpeed] = useState(10);

  useEffect(() => {
    if (!playing || !flight) return;
    const timer = setInterval(() => {
      const next = at + FRAME * speed;
      if (next >= flight.end) {
        onSeek(flight.end);
        setPlaying(false);
      } else {
        onSeek(next);
      }
    }, FRAME);
    return () => clearInterval(timer);
  }, [playing, flight, at, speed, onSeek]);

  const load = async () => {
    setLoading(true);
    setPlaying(false);
    setError(await onLoad(minutes));
    setLoading(false);
  };

  const p = flight ? pointAt(flight, at) : undefined;
  return (
    <>
      <h3>Replay</h3>
      <div className="row">
        <select aria-label="Replay range" value={minutes} onChange={(e) => setMinutes(Number(e.target.value))}>
          {RANGES.map(([label, m]) => (
            <option key={m} value={m}>
              {label}
            </option>
          ))}
        </select>
        <button onClick={load} disabled={loading}>
          {flight ? "Reload" : "Load"}
        </button>
      </div>
      {error && <p className="error">{error}</p>}
      {flight && flight.points.length === 0 && <p className="hint">Nothing stored for that time.</p>}
      {flight && flight.points.length > 0 && (
        <>
          <input
            className="scrubber"
            aria-label="Replay time"
            type="range"
            min={flight.start}
            max={flight.end}
            step={1000}
            value={at}
            onChange={(e) => onSeek(Number(e.target.value))}
          />
          <p className="hint" data-testid="replay-point" data-points={flight.points.length}>
            {new Date(at).toLocaleTimeString()}
            {p && ` · ${p.mode ?? "?"} · ${p.armed ? "armed" : "disarmed"} · ${p.rel_alt_m.toFixed(1)} m · ${p.battery_pct ?? "?"}%`}
          </p>
          <div className="row">
            <button
              onClick={() => {
                if (!playing && at >= flight.end) onSeek(flight.start);
                setPlaying(!playing);
              }}
            >
              {playing ? "Pause" : "Play"}
            </button>
            <select aria-label="Replay speed" value={speed} onChange={(e) => setSpeed(Number(e.target.value))}>
              {SPEEDS.map((s) => (
                <option key={s} value={s}>
                  {s}×
                </option>
              ))}
            </select>
            <button onClick={onClose}>Close</button>
          </div>
        </>
      )}
    </>
  );
}
