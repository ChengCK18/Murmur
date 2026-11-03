import { useEffect, useRef } from "react";
import { LngLatBounds, Map as MapLibreMap, Marker } from "maplibre-gl";
import "maplibre-gl/dist/maplibre-gl.css";
import type { DroneState } from "./types";
import { altitudeLabel, batteryLabel } from "./drones";

// Free map tiles, no API key.
const STYLE = "https://tiles.openfreemap.org/styles/liberty";
const HOME: [number, number] = [149.165237, -35.363262]; // ArduPilot SITL home

interface Props {
  drones: DroneState[];
  selected: number | null;
  onSelect(id: number): void;
}

// DroneMap draws one marker per drone with a position: an arrow turned to
// the heading, labelled with altitude and battery, grey once the link is lost.
export function DroneMap({ drones, selected, onSelect }: Props) {
  const container = useRef<HTMLDivElement>(null);
  const map = useRef<MapLibreMap | null>(null);
  const markers = useRef(new Map<number, Marker>());
  const fitted = useRef(false);

  useEffect(() => {
    const m = new MapLibreMap({ container: container.current!, style: STYLE, center: HOME, zoom: 15 });
    map.current = m;
    return () => {
      m.remove();
      map.current = null;
      markers.current.clear();
    };
  }, []);

  useEffect(() => {
    const m = map.current;
    if (!m) return;
    const seen = new Set<number>();
    const bounds = new LngLatBounds();
    for (const d of drones) {
      if (!d.position) continue;
      seen.add(d.drone_id);
      const at: [number, number] = [d.position.lon, d.position.lat];
      bounds.extend(at);
      let mk = markers.current.get(d.drone_id);
      if (!mk) {
        mk = new Marker({ element: markerElement(d.drone_id, onSelect) }).setLngLat(at).addTo(m);
        markers.current.set(d.drone_id, mk);
      }
      mk.setLngLat(at);
      paint(mk.getElement(), d, d.drone_id === selected);
    }
    for (const [id, mk] of markers.current) {
      if (!seen.has(id)) {
        mk.remove();
        markers.current.delete(id);
      }
    }
    // Show the whole fleet once, when the first positions arrive.
    if (!fitted.current && !bounds.isEmpty()) {
      fitted.current = true;
      m.fitBounds(bounds, { padding: 80, maxZoom: 16, duration: 0 });
    }
  }, [drones, selected, onSelect]);

  // Centre on a drone picked in the list.
  useEffect(() => {
    const d = drones.find((x) => x.drone_id === selected);
    if (d?.position && map.current) {
      map.current.easeTo({ center: [d.position.lon, d.position.lat], duration: 500 });
    }
    // Only when the selection changes, not on every update.
  }, [selected]);

  return <div ref={container} className="map" />;
}

function markerElement(id: number, onSelect: (id: number) => void): HTMLElement {
  const el = document.createElement("div");
  el.className = "drone";
  el.dataset.droneId = String(id);
  el.innerHTML = `<svg class="arrow" viewBox="-10 -10 20 20" width="24" height="24">
      <path d="M0,-9 L7,8 L0,4 L-7,8 Z" /></svg><span class="label"></span>`;
  el.addEventListener("click", (e) => {
    e.stopPropagation();
    onSelect(id);
  });
  return el;
}

function paint(el: HTMLElement, d: DroneState, selected: boolean): void {
  const heading = d.position?.heading_deg ?? 0;
  (el.querySelector(".arrow") as SVGElement).style.transform = `rotate(${heading}deg)`;
  el.querySelector(".label")!.textContent = `${d.drone_id} · ${altitudeLabel(d)} · ${batteryLabel(d)}`;
  el.classList.toggle("lost", d.link === "lost");
  el.classList.toggle("selected", selected);
  el.dataset.lat = String(d.position?.lat ?? "");
  el.dataset.lon = String(d.position?.lon ?? "");
}
