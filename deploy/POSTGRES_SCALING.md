# Postgres scaling: connection pooling & read replicas

This guide covers the production Postgres topology for zk-drive at scale
(5000 SME tenants + B2C consumers): **PgBouncer** for connection pooling
between the app fleet and Postgres, and **read replicas** for offloading
`SELECT` traffic via `DATABASE_READ_URL`.

It complements the env-var reference in
[`docs/CONFIGURATION.md`](../docs/CONFIGURATION.md) — read the
"Sizing the pool across replicas" and "Read replicas" sections there
first.

---

## Why pooling is mandatory at scale

`DB_MAX_CONNS` is **per process**. Each `server` / `worker` replica opens
its own `pgxpool`, so the backend count Postgres sees is:

```
peak backends ≈ server_replicas × server.DB_MAX_CONNS
              + worker_replicas × worker.DB_MAX_CONNS
              + (migrate / reconciler / orphan-gc / audit-archiver, default sizing)
```

With the server HPA at 20 pods and `DB_MAX_CONNS=20`, that is already
**400** server-tier backends — well past a stock `max_connections=100`.
Postgres backends are expensive (several MB RAM each + per-backend
planning/snapshot overhead), so the answer is **not** simply raising
`max_connections`; it is to multiplex many cheap client connections onto
a small set of real backends with PgBouncer.

---

## Topology

```
                         ┌─────────────────────────────────────────┐
   writes + txns         │                                         │
 server/worker ──────────┤  PgBouncer (writes)  ──►  Postgres PRIMARY
   (DATABASE_URL)         │  transaction pooling     (read/write)   │
                         └─────────────────────────────────────────┘
                                                          │ streaming
                                                          │ replication
                         ┌─────────────────────────────────────────┐
   read-only SELECTs     │                                         ▼
 server ─────────────────┤  PgBouncer (reads)   ──►  Postgres REPLICA(s)
   (DATABASE_READ_URL)    │  transaction pooling     (hot standby)  │
                         └─────────────────────────────────────────┘
```

- **Primary** takes every mutation, every transaction (including
  read-only transactions — the `ReadWriteSplitter` never splits a unit
  of work), and any read that must be read-your-write consistent.
- **Replica(s)** take the high-volume read path: folder tree walks, file
  listings, version/tag lookups, search. Add more replicas to scale read
  throughput horizontally.
- **PgBouncer** sits in front of both so `replicas × DB_MAX_CONNS`
  becomes the *client* connection count (cheap) rather than the
  *backend* count (scarce).

The app routes automatically: set `DATABASE_URL` to the write PgBouncer
and `DATABASE_READ_URL` to the read PgBouncer. The splitter in
`internal/database/splitter.go` decides per statement (see
`docs/CONFIGURATION.md` → Read replicas for the exact classification).

---

## PgBouncer configuration

Use **transaction pooling** (`pool_mode = transaction`). zk-drive is
compatible with it because:

- Tenant isolation is bound per **connection acquire** via
  `SELECT set_config('app.workspace_id', …)` (see
  `internal/database/postgres.go` `bindTenantGUC`), and pgx re-runs that
  hook on every checkout — so a transaction-pooled backend always carries
  the correct `app.workspace_id` GUC for the duration of the transaction.
- The app does not rely on session-lifetime server state (no
  `SET`-and-reuse-later, no session-level advisory locks held across
  transactions; the migration advisory lock is held only inside
  `Migrate`, which runs against the **primary** via the one-shot
  `migrate` binary, not through PgBouncer).

> ⚠️ Do **not** use `pool_mode = statement`. It breaks multi-statement
> transactions and the per-connection GUC bind, which would defeat RLS
> tenant isolation.

### `pgbouncer.ini` (write pool → primary)

```ini
[databases]
zkdrive = host=postgres-primary port=5432 dbname=zkdrive

[pgbouncer]
listen_addr = 0.0.0.0
listen_port = 6432
auth_type = scram-sha-256
auth_file = /etc/pgbouncer/userlist.txt
pool_mode = transaction

; Real backends opened to Postgres PER PgBouncer instance.
; Keep (pgbouncer_replicas × default_pool_size) comfortably under
; the primary's max_connections, leaving headroom for the one-shot
; jobs (migrate/reconciler/orphan-gc/audit-archiver) that connect
; directly.
default_pool_size = 25
min_pool_size = 5
reserve_pool_size = 5
reserve_pool_timeout = 3

; Client side can be large and cheap — this is what the app fleet
; (replicas × DB_MAX_CONNS) connects into.
max_client_conn = 5000

server_idle_timeout = 600
server_lifetime = 3600
query_wait_timeout = 30
```

### Read pool → replica(s)

Identical to the above but point `[databases]` at the replica
service/endpoint:

```ini
[databases]
zkdrive = host=postgres-replica port=5432 dbname=zkdrive
```

If you run multiple replicas, either (a) run one read PgBouncer per
replica and load-balance at the service layer, or (b) point the read
PgBouncer at a replica load-balancer endpoint. Either way the app only
sees a single `DATABASE_READ_URL`.

### Backend budget worked example

```
primary max_connections        = 200
write PgBouncer instances       = 2
write default_pool_size         = 25   →  2 × 25 = 50 backends
reserve_pool_size               = 5    →  2 × 5  = 10 backends
one-shot jobs (direct, default) ≈ 20 backends
                                  ----
                                  ≈ 80 backends  ✓ (headroom under 200)
```

The **client** side (app fleet) can be 20 server pods × 20
`DB_MAX_CONNS` = 400 client connections — well under
`max_client_conn = 5000`, and they cost PgBouncer almost nothing.

---

## Provisioning the replica

A read replica is a standard Postgres streaming-replication hot standby:

```sql
-- On the primary: a dedicated replication role.
CREATE ROLE zkdrive_repl WITH REPLICATION LOGIN PASSWORD '…';
```

```bash
# On the replica host: base backup from the primary, then start as a
# hot standby (managed Postgres — RDS/Cloud SQL/Aurora — does this for
# you when you add a read replica; the steps below are for self-hosted).
pg_basebackup -h postgres-primary -U zkdrive_repl -D "$PGDATA" -Fp -Xs -P -R
# -R writes standby.signal + primary_conninfo so the node boots as a
# read-only hot standby that streams WAL from the primary.
```

Confirm `hot_standby = on` (default) so the replica accepts read-only
queries, and monitor replication lag (`pg_stat_replication` on the
primary, `pg_last_wal_replay_lag()` on the replica). zk-drive tolerates
modest lag because anything needing read-your-write runs in a
primary-pinned transaction, but you should still alert on lag growth as
a replication-health signal.

---

## Kubernetes / Helm wiring

Point the env vars at the two PgBouncer Services:

```yaml
env:
  - name: DATABASE_URL          # write pool → primary
    value: postgres://zkdrive@pgbouncer-write:6432/zkdrive?sslmode=disable
  - name: DATABASE_READ_URL     # read pool → replica(s)
    value: postgres://zkdrive@pgbouncer-read:6432/zkdrive?sslmode=disable
  # Optional: size the read pool independently of the primary. Unset →
  # inherits DB_MAX_CONNS / DB_MIN_CONNS (both pools identical). Reads
  # are offloaded asymmetrically, so a read-heavy fleet typically wants
  # a larger read pool than write pool.
  - name: DB_MAX_CONNS          # write pool size (per process)
    value: "20"
  - name: DB_READ_MAX_CONNS     # read pool size (per process)
    value: "60"
```

When sizing, apply the same per-process math to each pool separately:
peak primary backends ≈ `replicas × DB_MAX_CONNS`, and peak replica
backends ≈ `replicas × DB_READ_MAX_CONNS`. Size each PgBouncer's
`default_pool_size` / `max_db_connections` against its own pool's
worst case. `DB_READ_MAX_CONNS` is clamped to `[2, 200]` and
`DB_READ_MIN_CONNS` to `[0, DB_READ_MAX_CONNS]`; the read pool reuses
`DB_MAX_CONN_IDLE_TIME`.

`sslmode=disable` is fine **inside** the cluster mesh when traffic to
PgBouncer stays on a private network with mTLS at the mesh layer; use
`require`/`verify-full` for any connection that leaves the trust
boundary.

The one-shot binaries (`migrate`, `reconciler`, `orphan-gc`,
`audit-archiver`) should keep `DATABASE_URL` pointed at the **write**
pool (or directly at the primary) and ignore `DATABASE_READ_URL` — they
either mutate or need a consistent view, and they do not benefit from
replica offload.
