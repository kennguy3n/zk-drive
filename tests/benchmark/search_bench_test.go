package benchmark

import (
	"testing"
	"time"

	"github.com/kennguy3n/zk-drive/internal/search"
)

// searchP95Target is the workstream latency budget: p95 of a full-text
// query under 500ms at 1M files. The seed size defaults to a modest
// number so the benchmark is runnable on a laptop / CI box; set
// BENCH_SEARCH_FILES=1000000 on a perf rig to measure the spec scale.
const searchP95Target = 500 * time.Millisecond

// searchSeedDefault keeps a default run fast. The benchmark reports the
// actual corpus size it measured so a result is unambiguous about scale.
const searchSeedDefault = 5000

// BenchmarkSearchFTS seeds a workspace with a corpus of files whose
// names share a common token, then measures the latency distribution of
// the production FTS path (search.Service.Search → workspace-scoped
// to_tsvector @@ plainto_tsquery with trigram fallback). It reports
// p50/p95/p99 in milliseconds alongside the 500ms p95 target.
func BenchmarkSearchFTS(b *testing.B) {
	env := setupBench(b)
	folderID := env.rootFolder(b)

	corpus := envInt("BENCH_SEARCH_FILES", searchSeedDefault)
	seedStart := time.Now()
	env.seedFiles(b, folderID, corpus, "report")
	b.Logf("seeded %d files in %s", corpus, time.Since(seedStart).Round(time.Millisecond))

	// Default options: 'simple' regconfig (hits the unaccent GIN index),
	// no fuzzy. This is the common, fastest path and the one the target
	// is written against.
	opts := search.Options{}
	rec := &latencyRecorder{}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		hits, err := env.search.Search(env.wsCtx, env.wsID, "report", opts, 20, 0)
		dur := time.Since(start)
		if err != nil {
			b.Fatalf("search: %v", err)
		}
		rec.record(dur)
		if len(hits) == 0 {
			b.Fatalf("expected hits for seeded token; got 0 (corpus=%d)", corpus)
		}
	}
	b.StopTimer()
	rec.reportPercentiles(b, searchP95Target)
	b.ReportMetric(float64(corpus), "corpus-files")
}

// BenchmarkSearchFTSPaged measures deep-pagination latency (offset into
// the result set), which exercises the candidate-limit + ORDER BY path
// more heavily than a first-page query and is where FTS latency
// regressions usually first appear.
func BenchmarkSearchFTSPaged(b *testing.B) {
	env := setupBench(b)
	folderID := env.rootFolder(b)

	corpus := envInt("BENCH_SEARCH_FILES", searchSeedDefault)
	env.seedFiles(b, folderID, corpus, "invoice")

	opts := search.Options{}
	rec := &latencyRecorder{}
	offset := corpus / 2
	if offset > search.MaxOffset {
		offset = search.MaxOffset
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		if _, err := env.search.Search(env.wsCtx, env.wsID, "invoice", opts, 20, offset); err != nil {
			b.Fatalf("search: %v", err)
		}
		rec.record(time.Since(start))
	}
	b.StopTimer()
	rec.reportPercentiles(b, searchP95Target)
	b.ReportMetric(float64(corpus), "corpus-files")
}
