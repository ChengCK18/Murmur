package db

import (
	"context"
	"net/netip"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"

	"github.com/ChengCK18/murmur/internal/db/dbtest"
	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/registry"
)

func TestDronesSurviveRestart(t *testing.T) {
	pool := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := Drones{Pool: pool, TenantID: 1}
	portA := netip.MustParseAddrPort("127.0.0.1:42318")
	portB := netip.MustParseAddrPort("127.0.0.1:52299")
	portC := netip.MustParseAddrPort("127.0.0.1:60001")
	t0 := time.Unix(1793516806, 0)
	hb := func(src netip.AddrPort, sys uint8, at time.Duration) mavlink.Received {
		return mavlink.Received{ReceivedAt: t0.Add(at), Src: src, SysID: sys, CompID: 1, Msg: &ardupilotmega.MessageHeartbeat{}}
	}
	resolve := func(g *registry.Registry, r mavlink.Received, want registry.Outcome, id registry.ID) {
		t.Helper()
		res := g.Resolve(r)
		if res.Outcome != want || res.Drone.ID != id {
			t.Fatalf("%v sys %d: %v drone %d, want %v drone %d", r.Src, r.SysID, res.Outcome, res.Drone.ID, want, id)
		}
	}

	// First ingester run: two drones.
	g, err := registry.NewWithStore(ctx, 3*time.Second, store)
	if err != nil {
		t.Fatal(err)
	}
	resolve(g, hb(portA, 1, 0), registry.Registered, 1)
	resolve(g, hb(portB, 2, 0), registry.Registered, 2)

	// Second run: the same drones keep their IDs, a restarted drone 1 is
	// rebound from its new port, and a new drone gets the next free ID.
	g2, err := registry.NewWithStore(ctx, 3*time.Second, store)
	if err != nil {
		t.Fatal(err)
	}
	resolve(g2, hb(portB, 2, 10*time.Second), registry.Known, 2)
	resolve(g2, hb(portC, 1, 10*time.Second), registry.Rebound, 1)
	resolve(g2, hb(portA, 3, 10*time.Second), registry.Registered, 3)

	// The rebind is stored too.
	drones, err := store.Load(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(drones) != 3 || drones[0].Src != portC || drones[1].Src != portB || drones[2].SysID != 3 {
		t.Errorf("stored drones %+v", drones)
	}
	if g.StoreErrors()+g2.StoreErrors() != 0 {
		t.Errorf("store errors %d %d", g.StoreErrors(), g2.StoreErrors())
	}
}

func TestCreateIsIdempotent(t *testing.T) {
	pool := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	store := Drones{Pool: pool, TenantID: 1}
	d := registry.Drone{Src: netip.MustParseAddrPort("127.0.0.1:42318"), SysID: 1, LastHeartbeat: time.Unix(1793516806, 0)}
	a, err := store.Create(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	b, err := store.Create(ctx, d) // as if the first reply had been lost
	if err != nil || a != b {
		t.Errorf("second create: id %d err %v, want %d", b, err, a)
	}
}
