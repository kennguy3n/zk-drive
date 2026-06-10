package benchmark

import (
	"context"
	"encoding/json"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/kennguy3n/zk-drive/api/ws"
)

// wsFanoutSizes are the per-workspace connection counts the fan-out
// benchmark sweeps. They bracket a busy SME workspace (hundreds of
// concurrent tabs) so the cost curve of a single change-feed broadcast
// is visible as connections scale.
var wsFanoutSizes = []int{100, 1000, 5000}

// drainClients launches a goroutine per client that empties its send
// channel into a counter, modelling the writePump that production runs.
// Without a drain the fixed-size send buffer would fill and the Hub's
// slow-consumer policy would drop frames, which is not what the fan-out
// cost benchmark intends to measure. Returns a stop func.
func drainClients(clients []*ws.Client) (received func() int64, stop func()) {
	var (
		mu    sync.Mutex
		count int64
		wg    sync.WaitGroup
	)
	done := make(chan struct{})
	for _, c := range clients {
		wg.Add(1)
		go func(c *ws.Client) {
			defer wg.Done()
			ch := c.Send()
			for {
				select {
				case <-done:
					return
				case <-ch:
					mu.Lock()
					count++
					mu.Unlock()
				}
			}
		}(c)
	}
	return func() int64 { mu.Lock(); defer mu.Unlock(); return count },
		func() { close(done); wg.Wait() }
}

// BenchmarkWSWorkspaceFanout measures the latency of a single
// workspace-wide broadcast (the change-feed hot path:
// Hub.BroadcastJSONWorkspace) as the number of connected clients grows.
// Synthetic clients are registered with a nil conn — valid because the
// fan-out path only writes to each client's send channel and never
// touches the socket (the write pump, which we do not start, owns the
// socket). This exercises the real two-index Hub under its real mutex.
func BenchmarkWSWorkspaceFanout(b *testing.B) {
	for _, n := range wsFanoutSizes {
		n := n
		b.Run(sizeLabel(n), func(b *testing.B) {
			hubCtx, cancel := context.WithCancel(context.Background())
			defer cancel()
			hub := ws.NewHub()
			go hub.Run(hubCtx)

			wsID := uuid.New()
			clients := make([]*ws.Client, 0, n)
			for i := 0; i < n; i++ {
				c := ws.NewClient(hub, nil, wsID, uuid.New())
				hub.Register(c)
				clients = append(clients, c)
			}
			// Wait until the hub's async register loop has indexed every
			// client so the first measured broadcast hits the full set.
			waitForWorkspaceCount(b, hub, wsID, n)

			_, stop := drainClients(clients)
			defer stop()

			payload, _ := json.Marshal(map[string]string{"type": "change", "seq": "bench"})

			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				hub.BroadcastJSONWorkspace(wsID, payload)
			}
			b.StopTimer()

			// Per-broadcast latency and the implied per-delivery cost.
			nsPerBroadcast := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
			b.ReportMetric(nsPerBroadcast/1e6, "ms/broadcast")
			if n > 0 {
				b.ReportMetric(nsPerBroadcast/float64(n), "ns/delivery")
			}
			b.ReportMetric(float64(n), "clients")
		})
	}
}

// BenchmarkWSRegister measures the cost of registering and unregistering
// connections, the churn the Hub absorbs as B2C clients connect and drop
// at scale. Each iteration registers then unregisters one client so the
// hub does not grow without bound across b.N.
func BenchmarkWSRegister(b *testing.B) {
	hubCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hub := ws.NewHub()
	go hub.Run(hubCtx)
	wsID := uuid.New()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		c := ws.NewClient(hub, nil, wsID, uuid.New())
		hub.Register(c)
		hub.Unregister(c)
	}
	b.StopTimer()
	nsPerOp := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
	if nsPerOp > 0 {
		b.ReportMetric(1e9/nsPerOp, "register-unregister/s")
	}
}

// waitForWorkspaceCount blocks until the hub reports the expected client
// count for the workspace or a short deadline elapses (the register loop
// is asynchronous). Fails the benchmark if the clients never land.
func waitForWorkspaceCount(b *testing.B, hub *ws.Hub, wsID uuid.UUID, want int) {
	b.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if hub.WorkspaceClientCount(wsID) >= want {
			return
		}
		time.Sleep(time.Millisecond)
	}
	b.Fatalf("hub never reached %d clients (got %d)", want, hub.WorkspaceClientCount(wsID))
}

// sizeLabel renders a sub-benchmark name like "clients=1000".
func sizeLabel(n int) string {
	return "clients=" + strconv.Itoa(n)
}
