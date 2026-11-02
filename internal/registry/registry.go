// Package registry gives each drone a Murmur drone ID.
//
// A drone is a (UDP source address, MAVLink system ID) pair. In Phase 1 the
// IDs come from an in-memory counter; Phase 2 stores them in the drones table.
//
// Rules (guide step 1.3):
//   - Ground-station traffic is ignored: system ID 255, component ID 190
//     (MAV_COMP_ID_MISSIONPLANNER), or a HEARTBEAT of type MAV_TYPE_GCS.
//   - A drone is created only by a HEARTBEAT, so stray packets do not create
//     drones.
//   - Rebind (finding 4): a restarted SITL sends from a new UDP port. A
//     heartbeat from a new address with the same IP and system ID as a known
//     drone that has timed out takes over that drone's ID.
package registry

import (
	"net/netip"
	"slices"
	"sync"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"

	"github.com/ChengCK18/murmur/internal/mavlink"
)

const (
	gcsSysID  = 255
	gcsCompID = 190 // MAV_COMP_ID_MISSIONPLANNER
)

// ID is a Murmur drone ID.
type ID int64

// Drone is one registered drone.
type Drone struct {
	ID            ID
	Src           netip.AddrPort // current UDP source address
	SysID         uint8
	FirstSeen     time.Time
	LastHeartbeat time.Time
	Rebinds       int // times the drone moved to a new address
}

// Outcome says how a frame was resolved.
type Outcome int

const (
	Ignored      Outcome = iota // ground-station traffic
	Unregistered                // from an unknown sender and not a heartbeat
	Known                       // from a registered drone
	Registered                  // a heartbeat that created a drone
	Rebound                     // a heartbeat that moved a timed-out drone to this address
)

func (o Outcome) String() string {
	return [...]string{"ignored", "unregistered", "known", "registered", "rebound"}[o]
}

// Result is the outcome of Resolve.
type Result struct {
	Outcome Outcome
	Drone   Drone          // valid for Known, Registered and Rebound
	OldSrc  netip.AddrPort // for Rebound: the address the drone used before
}

type key struct {
	src   netip.AddrPort
	sysID uint8
}

// Registry maps senders to drones. It is safe for concurrent use.
type Registry struct {
	mu      sync.Mutex
	timeout time.Duration // a drone with no heartbeat for this long may be rebound
	nextID  ID
	drones  map[key]*Drone
}

// New returns an empty Registry. timeout is how long a drone must have been
// without a heartbeat before its ID can move to a new address; use the link
// timeout (linkstats.DefaultTimeout).
func New(timeout time.Duration) *Registry {
	return &Registry{timeout: timeout, nextID: 1, drones: map[key]*Drone{}}
}

// Resolve finds the drone that sent r, registering it if r is its first
// heartbeat.
func (g *Registry) Resolve(r mavlink.Received) Result {
	hb, isHeartbeat := r.Msg.(*ardupilotmega.MessageHeartbeat)
	if r.SysID == gcsSysID || r.CompID == gcsCompID || (isHeartbeat && hb.Type == ardupilotmega.MAV_TYPE_GCS) {
		return Result{Outcome: Ignored}
	}

	g.mu.Lock()
	defer g.mu.Unlock()

	k := key{src: r.Src, sysID: r.SysID}
	if d, ok := g.drones[k]; ok {
		if isHeartbeat {
			d.LastHeartbeat = r.ReceivedAt
		}
		return Result{Outcome: Known, Drone: *d}
	}
	if !isHeartbeat {
		return Result{Outcome: Unregistered}
	}

	if old, d := g.rebindCandidate(r); d != nil {
		delete(g.drones, old)
		oldSrc := d.Src
		d.Src = r.Src
		d.LastHeartbeat = r.ReceivedAt
		d.Rebinds++
		g.drones[k] = d
		return Result{Outcome: Rebound, Drone: *d, OldSrc: oldSrc}
	}

	d := &Drone{ID: g.nextID, Src: r.Src, SysID: r.SysID, FirstSeen: r.ReceivedAt, LastHeartbeat: r.ReceivedAt}
	g.nextID++
	g.drones[k] = d
	return Result{Outcome: Registered, Drone: *d}
}

// rebindCandidate returns the timed-out drone with the same IP and system ID
// as r that was heard from most recently, or nil.
func (g *Registry) rebindCandidate(r mavlink.Received) (key, *Drone) {
	var bestKey key
	var best *Drone
	for k, d := range g.drones {
		if d.SysID != r.SysID || d.Src.Addr() != r.Src.Addr() {
			continue
		}
		if r.ReceivedAt.Sub(d.LastHeartbeat) <= g.timeout {
			continue // still alive: two instances share an IP and system ID
		}
		if best == nil || d.LastHeartbeat.After(best.LastHeartbeat) {
			bestKey, best = k, d
		}
	}
	return bestKey, best
}

// Drones returns every registered drone, ordered by ID.
func (g *Registry) Drones() []Drone {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]Drone, 0, len(g.drones))
	for _, d := range g.drones {
		out = append(out, *d)
	}
	slices.SortFunc(out, func(a, b Drone) int { return int(a.ID - b.ID) })
	return out
}
