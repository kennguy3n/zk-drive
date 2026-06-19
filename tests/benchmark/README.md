# Performance benchmark suite (`tests/benchmark`)

Real, database-backed Go benchmarks for the platform's scaling targets.
Every benchmark drives the same service and repository layer the HTTP
handlers call — `file.Service`, `storage.Client`, `search.Service`,
`permission.Service`, and the WebSocket `Hub` — so the numbers reflect
production code, not a model of it. The database-backed benchmarks run
against a live Postgres reached through `TEST_DATABASE_URL`, with
row-level security enforced exactly as in the running service: the
`app.workspace_id` GUC is bound per pooled connection, so every query
is scoped to one workspace (`harness_test.go:1-21`, `:50-52`).

This is the harness that answers "does a single workspace stay fast as
the corpus and the connection count grow?" — the same per-workspace
isolation that keeps Northwind Trading's files invisible to Lakeside
Legal in the live product is the isolation each benchmark runs under.

## What each benchmark measures

Targets are encoded as constants next to each benchmark and reported
alongside the achieved number, so a run is self-documenting.

| Benchmark | Target | What it isolates | Source |
| --- | --- | --- | --- |
| `BenchmarkUploadMetadata{Serial,Concurrent,Burst}` | 1000 commits/s | The file-row metadata `INSERT` that registers a file before the client streams bytes straight to the gateway — the per-request server cost on the upload path, not the byte transfer. | `upload_bench_test.go:12-17` |
| `BenchmarkDownloadURL{Serial,Concurrent}` | 5000 URLs/s | Presigned-GET generation, a local HMAC-SHA256 signing operation with no gateway round-trip, so this is the pure signer cost per download request. | `download_bench_test.go:10-15` |
| `BenchmarkSearchFTS{,Paged}` | p95 < 500 ms @ 1M files | Workspace-scoped full-text search through `search.Service`. Reports p50/p95/p99 in milliseconds. The paged variant exercises the deep-offset `ORDER BY` path where latency regressions surface first. | `search_bench_test.go:10-24`, `:58-62` |
| `BenchmarkPermissionResolutionUncached` | p95 < 50 ms (internal) | The cache-miss cold path: resolving an inherited permission at a depth-10 folder tree shared by 100 grantees — the work the permission cache exists to amortise. | `permission_bench_test.go:13-26` |
| `BenchmarkWSWorkspaceFanout` | fan-out latency vs. connections | A single change-feed broadcast (`Hub.BroadcastJSONWorkspace`) across 100, 1000, and 5000 in-memory clients. Reports ms/broadcast and ns/delivery. | `ws_bench_test.go:16-20`, `:55-102` |
| `BenchmarkWSRegister` | connect/disconnect churn | Register-then-unregister throughput — the churn the Hub absorbs as clients connect and drop at scale. | `ws_bench_test.go:104-126` |

The targets come from these source constants, not a separate spec file:
`uploadThroughputTarget = 1000` (`upload_bench_test.go:17`),
`downloadURLTarget = 5000` (`download_bench_test.go:15`),
`searchP95Target = 500ms` (`search_bench_test.go:14`), and
`permissionP95Target = 50ms` (`permission_bench_test.go:26`).

## How the harness works

Every benchmark starts from `setupBench`, which connects to
`TEST_DATABASE_URL`, runs the migrations, wires the production
services, and creates one freshly-named workspace and owner user for
the run (`harness_test.go:71-122`):

- **Isolated, synthetic data.** Each run provisions its own
  `bench-<uuid>` workspace and a throwaway `Bench Owner`
  (`harness_test.go:97-106`). The suite never reads or mutates the
  demo seed, so benchmarks are repeatable and leave no trace in a
  workspace you care about.
- **Row-level security on.** Per-tenant calls run with `wsCtx`, which
  binds the `app.workspace_id` GUC on the pooled connection, so the
  measured query path includes RLS exactly as production does
  (`harness_test.go:50-52`, `:112`).
- **No S3 round-trip.** The storage client is presign-only: its
  endpoint is never dialled because `GenerateDownloadURL` signs
  locally, so a placeholder endpoint and demo credentials are
  sufficient and the download benchmark stays free of network latency
  (`harness_test.go:124-144`).
- **Skip, don't fail, without a database.** When `TEST_DATABASE_URL`
  is unset the database-backed benchmarks skip rather than fail
  (`harness_test.go:73-76`), so `go test ./...` and `go vet ./...`
  stay green on a box with no Postgres. The WebSocket benchmarks need
  no database and always run.

## Running

```bash
# The same DSN the integration suite uses.
export TEST_DATABASE_URL='postgres://zkdrive:zkdrive@localhost:5432/zk-drive?sslmode=disable'

# Run every benchmark. The -run '^$' matches no unit tests, so only
# benchmarks fire; -benchtime 10x fixes the iteration count.
go test ./tests/benchmark/... -run '^$' -bench . -benchtime 10x

# One benchmark, more iterations, with allocation counts.
go test ./tests/benchmark/... -run '^$' -bench BenchmarkSearchFTS -benchtime 50x -benchmem
```

The DSN above is the value CI sets for the integration job
(`.github/workflows/ci.yml:191`); point it at any migrated Postgres
the suite can reach. Migrations are applied automatically — the
harness walks up from its own location to find the `migrations`
directory, so it runs from any working directory
(`harness_test.go:193-212`).

## Reading the results

Each benchmark reports the achieved figure **next to** its target via
`b.ReportMetric`, so you compare them on one line without consulting
the spec:

- **Throughput benchmarks** print `ops/s` (or `rows/s` for the burst
  variant) alongside `target-ops/s` (`harness_test.go:219-227`,
  `upload_bench_test.go:85-90`).
- **Latency benchmarks** print `p50-ms`, `p95-ms`, and `p99-ms`
  alongside `target-p95-ms`; the search benchmark also echoes the
  `corpus-files` it measured so a result is unambiguous about scale
  (`harness_test.go:238-257`, `search_bench_test.go:54-55`).
- **The fan-out benchmark** prints `ms/broadcast`, `ns/delivery`, and
  `clients` per connection-count sub-benchmark
  (`ws_bench_test.go:93-99`).

A run looks like this (numbers vary by hardware):

```text
BenchmarkUploadMetadataConcurrent-8   1240 ops/s   1000 target-ops/s
BenchmarkSearchFTS-8                  18.2 p50-ms   41.7 p95-ms   63.0 p99-ms   500 target-p95-ms   5000 corpus-files
BenchmarkWSWorkspaceFanout/clients=1000-8   0.83 ms/broadcast   831 ns/delivery   1000 clients
```

The suite intentionally **does not fail** when a target is missed. A
shared CI box is not a performance rig, so the benchmarks surface the
numbers for a human or a dedicated performance job to compare; treat a
regression as a signal, not a red build (`harness_test.go:214-227`).

## Tuning knobs (environment variables)

| Variable | Default | Effect |
| --- | --- | --- |
| `TEST_DATABASE_URL` | unset | Postgres DSN. Unset means the database-backed benchmarks skip. |
| `BENCH_SEARCH_FILES` | 5000 | Search corpus seeded before the FTS benchmarks. Set it to `1000000` on a performance rig to measure the 1M-file scale the target is written against (`search_bench_test.go:10-18`, `harness_test.go:179-191`). |
| `S3_ENDPOINT` / `S3_BUCKET` / `S3_ACCESS_KEY` / `S3_SECRET_KEY` | placeholder | Only used to construct the presign client; the endpoint is never dialled, so the defaults are fine (`harness_test.go:128-143`). |

The corpus seed runs before the timer starts — `seedFiles` inserts the
files and the benchmark calls `b.ResetTimer` afterwards, so the seed
cost is never counted in the measured region
(`harness_test.go:157-169`, `search_bench_test.go:29-40`).

## CI behavior

The benchmark package is compiled and vetted on every push but its
benchmarks are not run there:

- The `backend` job's `go test -race -count=1 -short ./...`
  (`.github/workflows/ci.yml:74-75`) compiles `tests/benchmark` and
  runs its (zero) unit tests, which proves the package builds without
  spending CI time on benchmarks — `go test` never runs `Benchmark`
  functions unless invoked with `-bench`.
- The integration job provisions Postgres and runs
  `go test ./tests/integration/...`
  (`.github/workflows/ci.yml:242-243`); it does not invoke the
  benchmarks.

There is no dedicated performance job by design: the benchmarks
produce meaningful numbers only on a quiet, provisioned machine, so
they are run on demand with `-bench` rather than gated in CI.

## Layout

```
tests/benchmark/
├─ harness_test.go            # setupBench fixture, metric reporters, helpers
├─ upload_bench_test.go       # file-row metadata commit throughput
├─ download_bench_test.go     # presigned-URL signing throughput
├─ search_bench_test.go       # workspace-scoped FTS latency (p50/p95/p99)
├─ permission_bench_test.go   # inherited-permission cold-path latency
└─ ws_bench_test.go           # change-feed fan-out + connection churn
```
