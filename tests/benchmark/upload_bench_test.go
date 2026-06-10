package benchmark

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/google/uuid"
)

// uploadThroughputTarget is the workstream goal: sustain 1000 upload
// metadata commits per second. The "upload" hot path on the API server
// is the metadata INSERT that registers a file row before the client
// streams bytes straight to the gateway — that DB write, not the byte
// transfer, is what the API pod must sustain, so it is what we measure.
const uploadThroughputTarget = 1000.0

// BenchmarkUploadMetadataSerial measures single-goroutine file-row
// creation latency (the per-request server cost).
func BenchmarkUploadMetadataSerial(b *testing.B) {
	env := setupBench(b)
	folderID := env.rootFolder(b)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		name := fmt.Sprintf("upload-%d-%s.bin", i, uuid.NewString())
		if _, err := env.files.Create(env.wsCtx, env.wsID, folderID, name, "application/octet-stream", env.ownerID); err != nil {
			b.Fatalf("create: %v", err)
		}
	}
	b.StopTimer()
	reportThroughput(b, uploadThroughputTarget)
}

// BenchmarkUploadMetadataConcurrent measures aggregate throughput under
// the concurrency the 1000-uploads/s target implies. b.RunParallel
// spreads the work across GOMAXPROCS goroutines, each holding its own
// pooled connection, so the number reflects the pool + Postgres under
// contention rather than a single serial connection.
func BenchmarkUploadMetadataConcurrent(b *testing.B) {
	env := setupBench(b)
	folderID := env.rootFolder(b)
	var counter int64

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			n := atomic.AddInt64(&counter, 1)
			name := fmt.Sprintf("upload-c-%d-%s.bin", n, uuid.NewString())
			if _, err := env.files.Create(env.wsCtx, env.wsID, folderID, name, "application/octet-stream", env.ownerID); err != nil {
				b.Fatalf("create: %v", err)
			}
		}
	})
	b.StopTimer()
	reportThroughput(b, uploadThroughputTarget)
}

// BenchmarkUploadMetadataBurst models the realistic burst the API sees
// when many clients confirm uploads at once: a fixed fan-out of
// goroutines each committing a batch. It reports aggregate ops/s so the
// result is directly comparable to the 1000/s target.
func BenchmarkUploadMetadataBurst(b *testing.B) {
	env := setupBench(b)
	folderID := env.rootFolder(b)
	const fanout = 32

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		var wg sync.WaitGroup
		wg.Add(fanout)
		for g := 0; g < fanout; g++ {
			go func(g int) {
				defer wg.Done()
				name := fmt.Sprintf("burst-%d-%d-%s.bin", i, g, uuid.NewString())
				if _, err := env.files.Create(env.wsCtx, env.wsID, folderID, name, "application/octet-stream", env.ownerID); err != nil {
					b.Errorf("create: %v", err)
				}
			}(g)
		}
		wg.Wait()
	}
	b.StopTimer()
	// Each iteration committed `fanout` rows; scale ops/s accordingly.
	nsPerIter := float64(b.Elapsed().Nanoseconds()) / float64(b.N)
	if nsPerIter > 0 {
		b.ReportMetric(1e9/nsPerIter*float64(fanout), "rows/s")
		b.ReportMetric(uploadThroughputTarget, "target-ops/s")
	}
}
