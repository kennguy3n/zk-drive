# ZK Drive Deployment

**License**: Proprietary — All Rights Reserved.

Three deployment paths ship in this directory:

- **Helm chart** under `helm/` — parameterized, production-oriented
  install with HPA, PodDisruptionBudgets, NetworkPolicies, and a
  migration pre-upgrade hook. **Preferred for production.** See
  [Production deployment (Helm)](#production-deployment-helm).
- **Kubernetes** under `k8s/` — raw, namespace-scoped manifests for a
  full in-cluster install (Postgres StatefulSet, NATS, ClamAV, server,
  worker, Ingress) plus the autoscaling/disruption/network-policy
  objects (`hpa-*.yaml`, `pdb-*.yaml`, `networkpolicy.yaml`).
- **Docker Compose** at `docker-compose.prod.yml` — single-host
  deployment with resource limits, health checks, and volumes for
  durability.

## Kubernetes

The manifests target non-production clusters. For production, swap
Postgres and NATS for managed services (RDS / Cloud SQL and the
provider's NATS-as-a-service); the in-cluster versions are
single-replica and are intended for development and staging only.

```bash
kubectl apply -f deploy/k8s/namespace.yaml
kubectl apply -f deploy/k8s/configmap.yaml
# Edit secret.yaml first — placeholder values will not work.
kubectl apply -f deploy/k8s/secret.yaml
kubectl apply -f deploy/k8s/postgres.yaml
kubectl apply -f deploy/k8s/nats.yaml
kubectl apply -f deploy/k8s/clamav.yaml
kubectl apply -f deploy/k8s/server.yaml
kubectl apply -f deploy/k8s/worker.yaml
kubectl apply -f deploy/k8s/ingress.yaml
# Autoscaling, disruption budgets, and network policies:
kubectl apply -f deploy/k8s/hpa-server.yaml
kubectl apply -f deploy/k8s/hpa-worker.yaml
kubectl apply -f deploy/k8s/pdb-server.yaml
kubectl apply -f deploy/k8s/pdb-worker.yaml
kubectl apply -f deploy/k8s/networkpolicy.yaml
```

The HPAs require a running `metrics-server`; the NetworkPolicies require
a CNI that enforces policy (Calico, Cilium, Antrea, AWS VPC CNI with
policy enabled). On a CNI that ignores NetworkPolicy the objects apply
cleanly but are not enforced.

The server deployment applies migrations at boot (via
`migrations/*.up.sql`); run `kubectl logs -n zk-drive deploy/zk-drive-server`
after the first rollout to verify.

### TLS

`ingress.yaml` references a TLS secret `zk-drive-tls`. Create it via
cert-manager or manually from a PEM pair before applying the
ingress:

```bash
kubectl -n zk-drive create secret tls zk-drive-tls \
  --cert=fullchain.pem --key=privkey.pem
```

## Production deployment (Helm)

The chart under `helm/` is the recommended production path. It templates
every manifest in `k8s/` and adds the production-readiness objects:

- **HorizontalPodAutoscalers** — server scales 2→20 on 70% CPU; worker
  scales 2→10 on 70% CPU (or, with the Prometheus Adapter installed, on
  a `zkdrive_worker_jobs_total`-derived custom metric — set
  `worker.hpa.metric=custom`).
- **PodDisruptionBudgets** — `minAvailable: 1` for server and worker so
  node drains / cluster upgrades never take a tier fully offline.
- **NetworkPolicies** — default-deny ingress+egress with scoped allows
  (ingress→server:8080, Prometheus→server:8080 / worker:9091,
  server/worker→Postgres:5432 / Redis:6379 / NATS:4222 / S3:443,
  worker→ClamAV:3310, a batch-egress policy for the migrate Job and
  CronJobs (`app: zk-drive-batch`) covering the same backends, and DNS
  egress to kube-dns). The S3 egress (443) is what lets the server run
  its `/readyz` HeadBucket check and the worker reach zk-object-fabric
  for previews, scanning, orphan GC, and audit archiving; set the port
  via `networkPolicy.s3Port`.
- **Pod anti-affinity + topology spread** so replicas spread across
  nodes and availability zones.
- **Migration Job** as a `pre-install,pre-upgrade` hook so the schema is
  current before the Deployments roll, plus the reconciler / orphan-gc /
  audit-archiver CronJobs.

```bash
# Render and inspect first:
helm template zk-drive deploy/helm -n zk-drive

# Install (creates the namespace):
helm upgrade --install zk-drive deploy/helm \
  -n zk-drive --create-namespace \
  --set image.tag=0.1.0 \
  --set ingress.host=drive.example.com \
  --set secrets.data.JWT_SECRET=<32+ byte secret> \
  --set secrets.data.S3_BUCKET=zk-drive-prod \
  --set secrets.data.S3_ENDPOINT=https://s3.amazonaws.com \
  --set secrets.data.S3_ACCESS_KEY=... \
  --set secrets.data.S3_SECRET_KEY=...
```

All tunables live in `helm/values.yaml` (replica counts, resource
requests/limits, image tag, env vars, ingress class, TLS secret name,
StorageClass, HPA/PDB/NetworkPolicy toggles, and per-dependency enable
flags).

### Uninstall and cleanup

```bash
helm uninstall zk-drive -n zk-drive
```

`zk-drive-config` (ConfigMap) and `zk-drive-secrets` (Secret) are created
as `pre-install,pre-upgrade` hooks so the migration Job can resolve their
`envFrom` references on a first install (see the migration-hook note
above). Helm does not track hook resources in the release manifest, so
`helm uninstall` leaves them behind. Remove them manually if you want a
clean teardown (skip the Secret if you supplied your own via
`secrets.existingSecret`):

```bash
kubectl delete configmap zk-drive-config -n zk-drive
kubectl delete secret zk-drive-secrets -n zk-drive   # chart-managed Secret only
# The namespace itself (if created with --create-namespace) and any PVCs
# from the bundled Postgres are also retained by design — delete them
# explicitly when decommissioning:
kubectl delete namespace zk-drive
```

### Managed Postgres (recommended for production)

The in-cluster Postgres StatefulSet is single-replica with a local PVC
and is intended for dev/staging only. For production, use managed
Postgres (RDS / Cloud SQL / Cloud SQL for PostgreSQL) and disable the
bundled one.

Keep the credential-bearing `DATABASE_URL` out of the ConfigMap: set
`config.databaseUrlInSecret=true` so the chart drops `DATABASE_URL` from
`zk-drive-config` and instead sources it from the Secret. The
connection string then lives only in the Secret (which can be encrypted
at rest and is more tightly RBAC-scoped than a ConfigMap), and reaches
the server, worker, and migrate Job through the same `envFrom`.

```bash
# Credentials in a pre-provisioned Secret (preferred — populated by
# External Secrets Operator, Sealed Secrets, or your cloud's secret
# manager; the Secret must carry a DATABASE_URL key):
helm upgrade zk-drive deploy/helm -n zk-drive \
  --set postgres.enabled=false \
  --set config.databaseUrlInSecret=true \
  --set secrets.create=false \
  --set secrets.existingSecret=zk-drive-db

# Or let the chart manage the Secret (avoid plaintext --set in shell
# history; prefer -f a gitignored values file):
helm upgrade zk-drive deploy/helm -n zk-drive \
  --set postgres.enabled=false \
  --set config.databaseUrlInSecret=true \
  --set secrets.data.DATABASE_URL='postgres://zkdrive:***@db.internal:5432/zkdrive?sslmode=require'
```

> **Why not `--set config.DATABASE_URL=...`?** That renders the password
> into the `zk-drive-config` ConfigMap, which is unencrypted at rest and
> broadly readable via RBAC. `config.databaseUrlInSecret=true` is the
> production path. (The raw `k8s/configmap.yaml` carries the same dev
> default; when applying the raw manifests against a managed DB, move
> `DATABASE_URL` into `k8s/secret.yaml` instead.)

> **NetworkPolicy + managed endpoints.** The egress policies match the
> bundled Postgres / NATS / ClamAV by in-cluster `podSelector`, so they
> only permit traffic to pods carrying those labels. A managed Postgres
> or NATS (RDS, Cloud SQL, NGS) has no in-cluster pod to select, so the
> `podSelector` rule won't match its IP and the connection is dropped
> under default-deny. Redis and S3 are already allowed **port-only** for
> exactly this reason. When you point the app at a managed backend,
> relax that backend's rule to a port-only `egress` entry (drop the
> `to.podSelector`) — or, on Cilium/Calico, use a CIDR/FQDN selector
> scoped to the provider — in both `k8s/networkpolicy.yaml` and the
> chart's `templates/networkpolicy.yaml`.

### PgBouncer sidecar / connection pooling

Postgres caps `max_connections`, and the HPA can fan the server out to
20 pods (plus workers and CronJobs), so front the database with a
transaction-pooling proxy. Two patterns work with this chart:

- **Sidecar** — add a `pgbouncer` container to the server pod and point
  `config.DATABASE_URL` at `127.0.0.1:6432`. Each pod gets a local pool;
  PgBouncer fans a few upstream connections out to many app connections.
- **Central Deployment** — run a small PgBouncer Deployment + Service
  (e.g. `pgbouncer.zk-drive.svc:6432`) shared by all tiers, and set
  `config.DATABASE_URL` to it. Easier to size against the managed DB's
  connection ceiling. If you use NetworkPolicies, allow
  server/worker→pgbouncer:6432 and pgbouncer→Postgres:5432.

Use `pool_mode = transaction` for the server/worker request path. Point
the **migration Job** (`/app/migrate`) and any advisory-lock-holding
path at Postgres directly — bypass the transaction pooler — since the
migrate runner takes a Postgres advisory lock that spans multiple
statements (see `k8s/migrate-job.yaml`) and session-scoped locks don't
survive transaction-level pooling.

### NATS cluster (HA JetStream)

The bundled NATS is a single replica — fine for dev, but a restart drops
in-flight JetStream consumers until the worker reconnects. For
production run a clustered NATS with JetStream file storage so streams
survive a node loss:

- Deploy the upstream **`nats` Helm chart** with
  `cluster.enabled=true`, `cluster.replicas=3`, and
  `jetstream.enabled=true` backed by a PVC per replica.
- Disable the bundled NATS here (`--set nats.enabled=false`) and point
  `config.NATS_URL` at the cluster service, e.g.
  `nats://nats.messaging.svc.cluster.local:4222`.
- Size JetStream replicas to 3 for the streams the worker consumes so a
  single node failure doesn't lose acknowledged messages. If using
  NetworkPolicies across namespaces, allow server/worker→NATS:4222 to
  the messaging namespace.

## Docker Compose (production-oriented)

`docker-compose.prod.yml` is a single-host compose file with:

- services bound to `127.0.0.1` only,
- named `pgdata` volume for Postgres,
- health checks on every service,
- resource limits (`cpus` / `memory`).

Front it with a reverse proxy (nginx, Caddy, Traefik) terminating TLS
and forwarding to `127.0.0.1:8080`.

```bash
# First time only — set secrets in a local .env file:
cat > .env <<'ENV'
POSTGRES_PASSWORD=changeme
JWT_SECRET=at-least-32-bytes-please-change-me
S3_BUCKET=zk-drive-prod
S3_ENDPOINT=https://s3.amazonaws.com
S3_ACCESS_KEY=...
S3_SECRET_KEY=...
ENV

docker compose -f deploy/docker-compose.prod.yml --env-file .env up -d
```

## Applying migrations manually

Both paths expect the server to run migrations automatically on boot.
If you need to run them ad hoc (e.g. a hotfix migration before a
rollout):

```bash
for f in migrations/*.up.sql; do
  psql "$DATABASE_URL" -f "$f"
done
```

## Notes and Caveats

- The raw `k8s/` manifests now ship HorizontalPodAutoscalers
  (`hpa-*.yaml`), PodDisruptionBudgets (`pdb-*.yaml`), and
  NetworkPolicies (`networkpolicy.yaml`). The Helm chart parameterizes
  all of these; prefer it for production (see
  [Production deployment (Helm)](#production-deployment-helm)).
- The in-cluster Postgres StatefulSet is a single replica with a 20 GiB
  PVC. It is suitable for non-production environments only; production
  deployments should use managed Postgres (RDS / Cloud SQL).
- ClamAV's signature database is downloaded on each pod start. In
  production, use an init container or a shared PVC to reduce cold-start
  time.
