package main

import (
	"context"
	"fmt"
	"log"

	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/command"
	"github.com/ChengCK18/murmur/internal/state"
)

// flyingAbove is the height above home, in metres, from which a drone counts
// as flying. ArduPilot's own landed state is not among the messages streamed.
const flyingAbove = 0.5

// precondition says why a drone in state d cannot take a command, or "" if it
// can (plan §4). These checks are there to give a clear message early, not to
// keep the drone safe: the drone has the final say.
func precondition(typ string, d *state.Drone) string {
	flying := d.Position != nil && d.Position.RelAlt > flyingAbove
	switch typ {
	case command.Disarm:
		if flying {
			return "Disarm needs the drone on the ground"
		}
	case command.Takeoff:
		if !d.Armed || d.Mode != "GUIDED" {
			return "Takeoff needs the drone armed and in GUIDED mode"
		}
	case command.ReturnHome:
		if !d.Armed || !flying {
			return "Return to launch needs the drone armed and flying"
		}
	case command.Land:
		if !d.Armed || !flying {
			return "Land needs the drone armed and flying"
		}
	case command.MissionStart:
		if !d.Armed {
			return "Start mission needs the drone armed"
		}
	}
	return ""
}

// checker reads the latest state the consumer keeps in Redis.
type checker struct {
	rdb    *redis.Client
	tenant int64
}

func (c checker) check(ctx context.Context, req command.Request) string {
	if req.Type == command.Arm || req.Type == command.SetMode {
		return "" // the plan's table has no precondition for these
	}
	hash, err := c.rdb.HGetAll(ctx, fmt.Sprintf("drone:%d:%d", c.tenant, req.DroneID)).Result()
	if err != nil {
		log.Printf("drone %d state: %v", req.DroneID, err)
		return "the drone's state is not available"
	}
	if len(hash) == 0 {
		return "no state for this drone yet"
	}
	d, err := state.FromRedis(c.tenant, req.DroneID, hash)
	if err != nil {
		log.Printf("drone %d state: %v", req.DroneID, err)
		return "the drone's state is not available"
	}
	return precondition(req.Type, d)
}
