package command

import (
	"bufio"
	"os"
	"strconv"
	"strings"
	"testing"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
)

func TestItemsMatchAMissionArduPilotFlies(t *testing.T) {
	// testdata/missions/square.txt is the mission recorded in flight.tlog.
	f, err := os.Open("../../testdata/missions/square.txt")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	s := bufio.NewScanner(f)
	s.Scan() // the header
	var want [][]float64
	for s.Scan() {
		var row []float64
		for _, c := range strings.Fields(s.Text()) {
			v, err := strconv.ParseFloat(c, 64)
			if err != nil {
				t.Fatal(err)
			}
			row = append(row, v)
		}
		want = append(want, row)
	}

	req := MissionRequest{AltitudeM: 20, Waypoints: []Waypoint{
		{-35.362262, 149.165237}, {-35.362262, 149.166437}, {-35.363262, 149.166437},
	}}
	got := req.Items()
	if len(got) != len(want) {
		t.Fatalf("%d items, want %d", len(got), len(want))
	}
	for i := 1; i < len(want); i++ { // item 0 is home: ArduPilot fills it in
		w, g := want[i], got[i]
		if g.Seq != int(w[0]) || g.Frame != uint8(w[2]) || g.Command != uint16(w[3]) ||
			g.Lat != w[8] || g.Lon != w[9] || g.Alt != w[10] {
			t.Errorf("item %d: %+v, want %v", i, g, w)
		}
	}
}

func TestMissionPackets(t *testing.T) {
	m, err := CountCommand(Command{}, 6).Message(7, 1)
	if err != nil {
		t.Fatal(err)
	}
	if c, ok := m.(*ardupilotmega.MessageMissionCount); !ok || c.Count != 6 || c.TargetSystem != 7 {
		t.Errorf("%+v", m)
	}
	m, err = ItemCommand(Command{}, Item{Seq: 2, Command: 16, Frame: 3, Lat: -35.5, Lon: 149.25, Alt: 20}).Message(7, 1)
	if err != nil {
		t.Fatal(err)
	}
	i, ok := m.(*ardupilotmega.MessageMissionItemInt)
	if !ok || i.Seq != 2 || i.X != -355000000 || i.Y != 1492500000 || i.Z != 20 || i.Autocontinue != 1 {
		t.Errorf("%+v", m)
	}
}

func TestBrowsersCannotSendUploadPackets(t *testing.T) {
	for _, typ := range []string{missionCount, missionItem, Upload} {
		if Validate(typ, Params{}) == nil {
			t.Errorf("%s accepted as a command", typ)
		}
	}
}

func TestValidateMission(t *testing.T) {
	ok := []Waypoint{{-35.36, 149.16}}
	bad := map[string]MissionRequest{
		"no waypoints": {AltitudeM: 20},
		"no altitude":  {Waypoints: ok},
		"off the map":  {AltitudeM: 20, Waypoints: []Waypoint{{95, 0}}},
		"no fix":       {AltitudeM: 20, Waypoints: []Waypoint{{0, 0}}},
		"too many":     {AltitudeM: 20, Waypoints: make([]Waypoint, MaxWaypoints+1)},
	}
	for name, r := range bad {
		if r.Validate() == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if err := (MissionRequest{AltitudeM: 20, Waypoints: ok}).Validate(); err != nil {
		t.Error(err)
	}
}
