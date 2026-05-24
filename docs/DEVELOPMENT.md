# Development Guide

This document covers running ZK Drive locally, executing the test
suites, and the tooling expectations for contributing to the
codebase.

## Local stack

The top-level `docker-compose.yml` brings up the one-command local
stack:

- `postgres` — Postgres 16 with the `zkdrive` role and `zk-drive`
  database.
- `nats` — NATS JetStream broker for the preview / scan / index /
  classify / retention / archive / webhook workers.
- `clamav` — ClamAV daemon (`clamav/clamav:1.3`) used by the scan
  worker over INSTREAM.

```
docker compose up -d postgres nats clamav
```

The server and worker can then run as native Go processes (faster
iteration than rebuilding the image each time):

```
export DATABASE_URL=postgres://zkdrive:zkdrive@localhost:5432/zk-drive?sslmode=disable
export JWT_SECRET=dev-secret
export NATS_URL=nats://localhost:4222
export CLAMAV_ADDRESS=localhost:3310

export S3_ENDPOINT=http://localhost:8080
export S3_BUCKET=mybucket
export S3_ACCESS_KEY=demo-access-key
export S3_SECRET_KEY=demo-secret-key

go run ./cmd/migrate
go run ./cmd/server &
go run ./cmd/worker &
```

Point a browser at `http://localhost:8080` and sign up the first
admin user.

For a fully containerised local stack (server and worker inside
Compose), `docker-compose.yml` already defines the `server` and
`worker` services — `docker compose up -d` brings everything up at
once.

## Go tests

### Unit tests

```
go test -short ./...
```

`-short` skips the integration tests that require Postgres and an
S3 endpoint.

### Integration tests (requires Postgres)

```
docker compose up -d postgres
export DATABASE_URL=postgres://zkdrive:zkdrive@localhost:5432/zk-drive?sslmode=disable
export JWT_SECRET=dev-secret
go test ./tests/integration/ -v
```

### Integration tests with storage (requires zk-object-fabric)

```
export S3_ENDPOINT=http://localhost:8080
export S3_BUCKET=mybucket
export S3_ACCESS_KEY=demo-access-key
export S3_SECRET_KEY=demo-secret-key
go test ./tests/integration/ -v
```

The integration harness applies migrations from a clean schema on
each run and truncates all tables between tests via the shared
`ResetTables` helper.

### Lint and vet

```
go vet ./...
golangci-lint run
```

The CI runs both with the race detector enabled
(`go test -race ./...`) on every PR.

## Frontend

The frontend is a React + TypeScript SPA built with Vite, packaged
as a Progressive Web App.

```
cd frontend
npm install
npm run lint
npm run build
```

The dev server:

```
npm run dev
```

Points at `http://localhost:8080` by default; override with
`VITE_API_BASE_URL=http://your-api-host npm run dev`.

## Playwright end-to-end tests

The full browser-driven flow (login, upload, preview, sharing,
admin) lives in `frontend/e2e/`:

```
cd frontend
npx playwright install --with-deps   # first time only
npx playwright test
```

The tests bring up their own server / worker pair via
`playwright.config.ts`'s webServer block. A `--headed` flag is
useful for debugging:

```
npx playwright test --headed --debug
```

## Migrations

SQL migrations live under `migrations/` and are applied by the
`/app/migrate` binary (or `go run ./cmd/migrate` locally). They are
forward-only; new migrations get a sequential numeric prefix
(`NNN_short_name.up.sql` / `.down.sql`).

The down migrations exist so a developer can drop a migration
locally while iterating, and so CI exercises the down path against
the same Postgres image. They are **not** run in production
rollback — that's a database-level restore.

The migrator acquires a Postgres advisory lock keyed on a fixed
64-bit constant, so two pods running `migrate` concurrently during
a blue/green deploy serialise safely.

## Pre-commit hooks

The repo's `.pre-commit-config.yaml` runs `gofmt`, `go vet`, and
the basic frontend lint hooks on staged files. Install once:

```
pre-commit install
```

After that, `git commit` will reject changes that fail lint /
formatting locally — the same gates run again in CI on push.

## Branch and PR conventions

- Branch off `main`. CI is configured to require green checks on
  every PR before merge.
- Conventional Commits style for the PR title (`feat(drive): …`,
  `fix(webhooks): …`, `docs(operations): …`).
- Reference the issue or ticket in the body when applicable, but
  keep code comments free of issue-tracker IDs — those belong in
  Git history and the issue tracker, not in source files.
