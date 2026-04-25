# zk-drive deployment

Two deploy paths ship in this directory:

- **Kubernetes** under `k8s/` — namespace-scoped manifests for a full
  in-cluster install (Postgres StatefulSet, NATS, ClamAV, server,
  worker, Ingress).
- **Docker Compose** at `docker-compose.prod.yml` — single-host
  deployment with resource limits, health checks, and volumes for
  durability.

## Kubernetes

The manifests target dev / staging clusters. **For production, swap
Postgres and NATS for managed services** (RDS / Cloud SQL and the
provider's NATS-as-a-service) — the in-cluster versions are single-
replica and meant for cheap staging.

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
```

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

## Notes / caveats

- The Kubernetes manifests are **minimal**: no HPA, no PodDisruption-
  Budgets, no NetworkPolicies. Production overlays should add those
  via Kustomize or Helm.
- The in-cluster Postgres StatefulSet is a single replica with a 20GiB
  PVC. It is suitable for staging only. For production use RDS /
  Cloud SQL.
- ClamAV's signature database is downloaded on each pod start — in
  production, use an init container or a shared PVC to reduce cold
  starts.
