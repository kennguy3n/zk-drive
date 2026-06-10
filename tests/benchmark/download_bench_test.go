package benchmark

import (
	"fmt"
	"testing"

	"github.com/kennguy3n/zk-drive/internal/storage"
)

// downloadURLTarget is the workstream goal: 5000 presigned download-URL
// generations per second. Presigning is a local HMAC-SHA256 signing
// operation (no gateway round-trip), so this benchmark isolates the CPU
// cost of the signer — exactly the work an API pod does per download
// request before handing the URL back to the client.
const downloadURLTarget = 5000.0

// BenchmarkDownloadURLSerial measures single-goroutine presign latency.
func BenchmarkDownloadURLSerial(b *testing.B) {
	env := setupBench(b)
	key := fmt.Sprintf("ws/%s/obj/bench-object", env.wsID)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := env.storage.GenerateDownloadURL(env.wsCtx, key, storage.DefaultPresignExpiry); err != nil {
			b.Fatalf("presign: %v", err)
		}
	}
	b.StopTimer()
	reportThroughput(b, downloadURLTarget)
}

// BenchmarkDownloadURLConcurrent measures aggregate presign throughput
// across GOMAXPROCS goroutines — the relevant figure for the 5000/s
// target since the signer is stateless and scales with cores.
func BenchmarkDownloadURLConcurrent(b *testing.B) {
	env := setupBench(b)
	key := fmt.Sprintf("ws/%s/obj/bench-object", env.wsID)

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := env.storage.GenerateDownloadURL(env.wsCtx, key, storage.DefaultPresignExpiry); err != nil {
				b.Fatalf("presign: %v", err)
			}
		}
	})
	b.StopTimer()
	reportThroughput(b, downloadURLTarget)
}
