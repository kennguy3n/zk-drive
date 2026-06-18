# Performance benchmark suite (`tests/benchmark`)

Real, DB-backed Go benchmarks for the platform's scaling targets. Each
benchmark drives the **production** service/repository layer (the same
code the HTTP handlers call), against a live Postgres reached through
`TEST_DATABASE_URL`, with row-level security enforced exactly as in
production (the tenant GUC is bound per pooled connection).

## Running

```bash
# Same DSN the integration suite uses.
export TEST_DATABASE_URL='postgres://zkdrive@127.0.0.1:5432/zkdrive_test?sslmode=disable'

# Run everything (regex matches nothing for -run so only benchmarks fire).
go test ./tests/benchmark/... -run '^$' -bench . -benchtime 10x

# A single benchmark, more iterations, with allocations.
go test ./tests/benchmark/... -run '^$' -bench BenchmarkSearchFTS -benchtime 50x -benchmem
```

Without `TEST_DATABASE_URL` the DB-backed benchmarks **skip** (they do
not fail), so `go test ./...` and `go vet ./...` stay green in
environments without a database. The WebSocket benchmarks need no
database and always run.

## What each benchmark measures and its target

| Benchmark | Target | Notes |
| --- | --- | --- |
| `BenchmarkUploadMetadata{Serial,Concurrent,Burst}` | 1000 uploads/s | Measures the file-row metadata commit — the per-request server cost on the upload path (bytes stream straight to the gateway, not through the API). |
| `BenchmarkDownloadURL{Serial,Concurrent}` | 5000 URL/s | Presigned GET generation is a local HMAC sign (no gateway round-trip), so this isolates the signer cost per download request. |
| `BenchmarkSearchFTS{,Paged}` | p95 < 500ms @ 1M files | Workspace-scoped FTS via `search.Service`. Seed size is `BENCH_SEARCH_FILES` (default 5000); set it to `1000000` on a perf rig to measure the spec scale. Reports p50/p95/p99 ms. |
| `BenchmarkPermissionResolutionUncached` | (internal) p95 < 50ms | Inherited permission resolution at a depth-10 folder tree with 100 grantees — the cache-miss cold path the permission cache amortises. |
| `BenchmarkWSWorkspaceFanout` | fan-out latency vs. connections | Single change-feed broadcast (`Hub.BroadcastJSONWorkspace`) across 100/1000/5000 in-memory clients. Reports ms/broadcast and ns/delivery. |
| `BenchmarkWSRegister` | connect/disconnect churn | Register+unregister throughput, the churn the Hub absorbs at B2C scale. |

The harness reports the achieved number **next to** the target via
`b.ReportMetric` (e.g. `ops/s` alongside `target-ops/s`, or
`p95-ms` alongside `target-p95-ms`). The suite intentionally does **not**
fail when a target is missed: a shared CI box is not a performance rig, so
the benchmarks surface the numbers for a human or a dedicated perf job to
compare. Treat regressions as signals, not red builds.

## Tuning knobs (env vars)

| Var | Default | Effect |
| --- | --- | --- |
| `BENCH_SEARCH_FILES` | 5000 | Search corpus size seeded before the FTS benchmarks. |
| `S3_ENDPOINT` / `S3_BUCKET` / `S3_ACCESS_KEY` / `S3_SECRET_KEY` | placeholder | Only used to construct the presign client; the endpoint is never dialled. |
