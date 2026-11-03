// Package linkstats counts lost frames and tracks link state for each MAVLink
// sender.
//
// Loss comes from sequence numbers: every frame carries a counter (0 to 255,
// then wrapping) that is separate for each sender and shared by all of its
// message types. Link state comes from heartbeats, sent once a second: no
// heartbeat within the timeout means the link is lost.
//
// Time is always passed in (frame arrival times, Tick's now), so tests run on
// recorded or invented times without sleeping.
package linkstats

import (
	"net/netip"
	"sync"
	"time"

	"github.com/ChengCK18/murmur/internal/mavlink"
)

// DefaultTimeout is how long without a heartbeat before a link counts as lost:
// three missed heartbeats at ArduPilot's 1 Hz.
const DefaultTimeout = 3 * time.Second

// bootRewind is how far time_boot_ms must go backwards to count as a sender
// restart. ArduPilot's boot clock never goes backwards in a healthy stream, so
// the margin only has to absorb packets reordered on the network.
const bootRewind = 1000 // ms

// Key identifies one sender on one link.
type Key struct {
	Src    netip.AddrPort
	SysID  uint8
	CompID uint8
}

// Stats is the state of one sender.
type Stats struct {
	Frames uint64 // frames received
	Lost   uint64 // frames missing from sequence gaps
	// Resets counts restarts of the sequence: gaps of 128 or more (reordering
	// or a sender restart), and the first frame after a silence longer than
	// the timeout or after time_boot_ms went backwards. None count as loss.
	Resets uint64

	LastSeq       uint8
	LastSeen      time.Time
	LastHeartbeat time.Time // zero until the first heartbeat
	LinkUp        bool      // a heartbeat arrived within the timeout
	LinkLosses    uint64    // times LinkUp went from true to false
}

// Event is a change of link state.
type Event int

const (
	None         Event = iota
	LinkUp             // first heartbeat from a sender
	LinkLost           // no heartbeat within the timeout
	LinkRestored       // heartbeat after LinkLost
)

func (e Event) String() string {
	switch e {
	case LinkUp:
		return "link up"
	case LinkLost:
		return "link lost"
	case LinkRestored:
		return "link restored"
	}
	return "none"
}

// Change is an Event for one sender.
type Change struct {
	Key   Key
	Event Event
}

type sender struct {
	Stats
	bootMs  uint32
	hasBoot bool
}

// Tracker tracks every sender. It is safe for concurrent use.
type Tracker struct {
	mu      sync.Mutex
	timeout time.Duration
	senders map[Key]*sender
}

// New returns a Tracker that declares a link lost after timeout without a
// heartbeat.
func New(timeout time.Duration) *Tracker {
	return &Tracker{timeout: timeout, senders: map[Key]*sender{}}
}

// Observe records one received frame and returns LinkUp or LinkRestored if
// it is a heartbeat that changes the link state, otherwise None.
func (t *Tracker) Observe(r mavlink.Received) Event {
	t.mu.Lock()
	defer t.mu.Unlock()

	k := Key{Src: r.Src, SysID: r.SysID, CompID: r.CompID}
	s, ok := t.senders[k]
	if !ok {
		s = &sender{}
		t.senders[k] = s
	} else {
		t.countGap(s, r)
	}
	s.Frames++
	s.LastSeq = r.Seq
	s.LastSeen = r.ReceivedAt
	if ms, ok := mavlink.TimeBootMs(r.Msg); ok {
		s.bootMs, s.hasBoot = ms, true
	}

	if !mavlink.IsHeartbeat(r.Msg) {
		return None
	}
	first := s.LastHeartbeat.IsZero()
	s.LastHeartbeat = r.ReceivedAt
	if s.LinkUp {
		return None
	}
	s.LinkUp = true
	if first {
		return LinkUp
	}
	return LinkRestored
}

// countGap compares r with the sender's previous frame.
func (t *Tracker) countGap(s *sender, r mavlink.Received) {
	// A sender that restarted begins again at sequence 0, which can look like
	// a small gap (SITL went 176 → 0 in testdata/restart.tlog, gap 79). Spot
	// the restart from the silence before it or from its boot clock instead.
	if r.ReceivedAt.Sub(s.LastSeen) > t.timeout {
		s.reset()
		return
	}
	if ms, ok := mavlink.TimeBootMs(r.Msg); ok && s.hasBoot && ms+bootRewind < s.bootMs {
		s.reset()
		return
	}
	gap := r.Seq - s.LastSeq - 1 // uint8 arithmetic wraps: 255 → 0 gives 0
	if gap >= 128 {
		s.reset()
		return
	}
	s.Lost += uint64(gap)
}

// reset starts the sequence afresh. The boot clock is forgotten too: after a
// restart detected from silence, the old clock would make the new sender's
// first timestamped frame look like a second restart.
func (s *sender) reset() {
	s.Resets++
	s.hasBoot = false
}

// Tick marks links lost whose last heartbeat is older than the timeout at now,
// and returns a LinkLost Change for each. Call it regularly, such as once a
// second.
func (t *Tracker) Tick(now time.Time) []Change {
	t.mu.Lock()
	defer t.mu.Unlock()

	var out []Change
	for k, s := range t.senders {
		if s.LinkUp && now.Sub(s.LastHeartbeat) > t.timeout {
			s.LinkUp = false
			s.LinkLosses++
			out = append(out, Change{Key: k, Event: LinkLost})
		}
	}
	return out
}

// Get returns the stats of one sender.
func (t *Tracker) Get(k Key) (Stats, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	s, ok := t.senders[k]
	if !ok {
		return Stats{}, false
	}
	return s.Stats, true
}

// Totals is the sum over all senders: frames, lost and resets, with LinkUp
// counting the senders whose link is up. Forgotten senders are not included.
type Totals struct {
	Frames, Lost, Resets, LinkLosses uint64
	LinkUp, Senders                  int
}

// Totals sums the stats of every tracked sender.
func (t *Tracker) Totals() Totals {
	t.mu.Lock()
	defer t.mu.Unlock()
	var tot Totals
	for _, s := range t.senders {
		tot.Frames += s.Frames
		tot.Lost += s.Lost
		tot.Resets += s.Resets
		tot.LinkLosses += s.LinkLosses
		if s.LinkUp {
			tot.LinkUp++
		}
		tot.Senders++
	}
	return tot
}

// Forget drops a sender, for example one whose drone now sends from a new
// address after a restart.
func (t *Tracker) Forget(k Key) {
	t.mu.Lock()
	defer t.mu.Unlock()
	delete(t.senders, k)
}
