// Command wsprobe stands in for browsers during a load test (guide step 4.4):
// one connection follows a map area and another follows drones by ID, so the
// gateway measures receive to send for both while it serves them. It only
// reads, and prints what it received when stopped.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

func main() {
	base := flag.String("gateway", "127.0.0.1:8080", "gateway host:port")
	maxIDs := flag.Int("drones", 100, "drones to follow by ID")
	flag.Parse()
	log.SetFlags(log.Ltime)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ids, err := droneIDs("http://"+*base+"/api/drones", *maxIDs)
	if err != nil {
		log.Fatalf("wsprobe: %v", err)
	}
	var messages, errs atomic.Uint64
	var wg sync.WaitGroup
	for _, req := range []map[string]any{
		{"type": "subscribe_area", "bbox": []float64{-180, -90, 180, 90}},
		{"type": "subscribe", "drones": ids},
	} {
		wg.Go(func() {
			if err := follow(ctx, "ws://"+*base+"/ws", req, &messages); err != nil && ctx.Err() == nil {
				errs.Add(1)
				log.Printf("wsprobe: %v", err)
			}
		})
	}
	wg.Wait()
	fmt.Printf("%d messages, %d connection errors\n", messages.Load(), errs.Load())
}

func droneIDs(url string, limit int) ([]int64, error) {
	resp, err := http.Get(url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var body struct {
		Drones []struct {
			ID int64 `json:"drone_id"`
		} `json:"drones"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		return nil, err
	}
	ids := []int64{}
	for _, d := range body.Drones {
		if len(ids) < limit {
			ids = append(ids, d.ID)
		}
	}
	return ids, nil
}

func follow(ctx context.Context, url string, req map[string]any, messages *atomic.Uint64) error {
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		return err
	}
	defer conn.CloseNow()      //nolint:errcheck
	conn.SetReadLimit(1 << 24) // a snapshot of the whole fleet
	if err := wsjson.Write(ctx, conn, req); err != nil {
		return err
	}
	for {
		if _, _, err := conn.Read(ctx); err != nil {
			return err
		}
		messages.Add(1)
	}
}
