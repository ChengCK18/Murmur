// Package alert defines what the alerter tells the rest of the platform
// (plan §4, open item 5; guide step 5.6).
//
//	alerter → alert.<drone_id> → gateway → browsers
//	gateway → alert_active → alerter: the alerts raised now
//	gateway → geofence_get, geofence_set → alerter; geofence_changed → gateway
package alert

import (
	"strconv"
	"time"
)

const (
	Subjects        = "alert.*"
	ActiveSubject   = "alert_active"
	GeofenceGet     = "geofence_get"
	GeofenceSet     = "geofence_set"
	GeofenceChanged = "geofence_changed"
)

// Subject is where a drone's alerts are published.
func Subject(droneID int64) string { return "alert." + strconv.FormatInt(droneID, 10) }

// Kinds of alert.
const (
	LowBattery = "low_battery"
	LinkLost   = "link_lost"
	Geofence   = "geofence"
)

// Alert is one alert raised, or cleared when Raised is false.
type Alert struct {
	DroneID int64     `json:"drone_id"`
	Kind    string    `json:"kind"`
	Raised  bool      `json:"raised"`
	Message string    `json:"message"`
	At      time.Time `json:"at"`
}

// Point is a corner of a geofence.
type Point struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// Fence is the area drones must stay inside. No points means no geofence.
type Fence struct {
	Polygon   []Point   `json:"polygon"`
	UpdatedBy string    `json:"updated_by,omitempty"`
	UpdatedAt time.Time `json:"updated_at,omitzero"`
}

// SetFence asks the alerter to replace the geofence.
type SetFence struct {
	Polygon []Point `json:"polygon"`
	User    string  `json:"user"`
}

// SetResult answers a SetFence.
type SetResult struct {
	Fence Fence  `json:"fence"`
	Error string `json:"error,omitempty"`
}

// Contains reports whether p is inside the polygon, by ray casting. Latitude
// and longitude are treated as flat, which is fine at the size of a flying
// area.
func Contains(polygon []Point, p Point) bool {
	in := false
	for i, j := 0, len(polygon)-1; i < len(polygon); j, i = i, i+1 {
		a, b := polygon[i], polygon[j]
		if (a.Lat > p.Lat) != (b.Lat > p.Lat) &&
			p.Lon < (b.Lon-a.Lon)*(p.Lat-a.Lat)/(b.Lat-a.Lat)+a.Lon {
			in = !in
		}
	}
	return in
}
