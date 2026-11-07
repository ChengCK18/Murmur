import { useEffect, useRef } from "react";
import { type GeoJSONSource, LngLatBounds, Map as MapLibreMap, Marker, setWorkerUrl } from "maplibre-gl";
import workerUrl from "maplibre-gl/dist/maplibre-gl-worker.mjs?url";
import "maplibre-gl/dist/maplibre-gl.css";
import type { BBox, DroneState, Waypoint } from "./types";
import { altitudeLabel, batteryLabel, followedArea } from "./drones";

// MapLibre loads and decodes tiles in a web worker, which it looks for next
// to its own file. Once bundled that file is our assets/index-*.js, where no
// worker exists, so no tile is ever fetched and the map stays blank. Importing
// the worker as a URL makes Vite emit it and tells MapLibre where it is.
setWorkerUrl(workerUrl);

// Free map tiles, no API key.
const STYLE = "https://tiles.openfreemap.org/styles/liberty";
const HOME: [number, number] = [149.165237, -35.363262]; // ArduPilot SITL home

interface Props {
  drones: DroneState[];
  selected: number | null;
  onSelect(id: number): void;
  onArea(bbox: BBox): void;
  route: Waypoint[]; // the mission being planned, or the one the drone holds
  planning: boolean; // clicks on the map add waypoints
  onPick(p: Waypoint): void;
}

const ROUTE = "mission-route";

// DroneMap draws one marker per drone with a position: an arrow turned to
// the heading, labelled with altitude and battery, grey once the link is lost.
// It reports the area it shows whenever that changes, so the page follows only
// the drones on screen.
export function DroneMap({ drones, selected, onSelect, onArea, route, planning, onPick }: Props) {
  const container = useRef<HTMLDivElement>(null);
  const map = useRef<MapLibreMap | null>(null);
  const markers = useRef(new Map<number, Marker>());
  const drawn = useRef(new Map<number, DroneState>()); // the state each marker shows
  const drawnSelected = useRef<number | null>(null);
  const fitted = useRef(false);
  const points = useRef<Marker[]>([]);
  const routeRef = useRef(route);
  routeRef.current = route;
  const pick = useRef<((p: Waypoint) => void) | null>(null);
  pick.current = planning ? onPick : null;

  useEffect(() => {
    const m = new MapLibreMap({ container: container.current!, style: STYLE, center: HOME, zoom: 15 });
    map.current = m;
    m.on("click", (e) => pick.current?.({ lat: e.lngLat.lat, lon: e.lngLat.lng }));
    m.on("load", () => {
      m.addSource(ROUTE, { type: "geojson", data: routeLine(routeRef.current) });
      m.addLayer({ id: ROUTE, type: "line", source: ROUTE, paint: { "line-color": "#d97706", "line-width": 3, "line-dasharray": [2, 1] } });
    });
    const report = () => {
      const b = m.getBounds();
      onArea(followedArea([b.getWest(), b.getSouth(), b.getEast(), b.getNorth()]));
    };
    m.on("moveend", report);
    report();
    return () => {
      m.remove();
      map.current = null;
      markers.current.clear();
      drawn.current.clear();
    };
  }, [onArea]);

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
      // A thousand drones change a few at a time: leave the others alone.
      const changed = drawnSelected.current !== selected && (d.drone_id === selected || d.drone_id === drawnSelected.current);
      if (drawn.current.get(d.drone_id) !== d || changed) {
        mk.setLngLat(at);
        paint(mk.getElement(), d, d.drone_id === selected);
        drawn.current.set(d.drone_id, d);
      }
    }
    drawnSelected.current = selected;
    for (const [id, mk] of markers.current) {
      if (!seen.has(id)) {
        mk.remove();
        markers.current.delete(id);
        drawn.current.delete(id);
      }
    }
    // Show the whole fleet once, when the first positions arrive.
    if (!fitted.current && !bounds.isEmpty()) {
      fitted.current = true;
      m.fitBounds(bounds, { padding: 80, maxZoom: 16, duration: 0 });
    }
  }, [drones, selected, onSelect]);

  // The route: a numbered marker per waypoint, joined by a line.
  useEffect(() => {
    const m = map.current;
    if (!m) return;
    points.current.forEach((p) => p.remove());
    points.current = route.map((w, i) => {
      const el = document.createElement("div");
      el.className = "waypoint";
      el.textContent = String(i + 1);
      return new Marker({ element: el }).setLngLat([w.lon, w.lat]).addTo(m);
    });
    (m.getSource(ROUTE) as GeoJSONSource | undefined)?.setData(routeLine(route));
  }, [route]);

  useEffect(() => {
    map.current?.getCanvas().classList.toggle("planning", planning);
  }, [planning]);

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

function routeLine(route: Waypoint[]): Parameters<GeoJSONSource["setData"]>[0] {
  return { type: "Feature", properties: {}, geometry: { type: "LineString", coordinates: route.map((w) => [w.lon, w.lat]) } };
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
