# ZK Drive — Terraform IaC (AWS & GCP)

**License**: Proprietary — All Rights Reserved.

Production-grade Terraform for deploying ZK Drive on managed cloud
services, targeting SME operators who do **not** want to run Kubernetes.
Two self-contained modules:

| Module | Compute | Postgres | Cache | Edge |
| --- | --- | --- | --- | --- |
| [`aws/`](aws/) | ECS Fargate | RDS Postgres 16 (Multi-AZ + replica) | ElastiCache Redis 7 | ALB + CloudFront |
| [`gcp/`](gcp/) | Cloud Run | Cloud SQL Postgres 16 (HA + replica) | Memorystore Redis | External HTTPS LB + Cloud CDN |

Both deploy the same image (`ghcr.io/kennguy3n/zk-drive`) with per-binary
entrypoint overrides (`/app/server`, `/app/worker`, and on AWS the
scheduled `/app/reconciler`, `/app/orphan-gc`, `/app/audit-archiver`
jobs). Object storage for user files is **not** provisioned here — that is
the zk-object-fabric S3 gateway, configured via the `S3_*` /
`FABRIC_CONSOLE_URL` variables. The S3/GCS buckets created by these
modules hold only the **frontend static assets** (`frontend/dist`) — public
web artifacts. Preview thumbnails are **not** stored there; like user files
they live in zk-object-fabric and are served through the authenticated app
(`internal/preview/preview.go`).

All resources are tagged/labelled `project=zk-drive` and
`environment=<var.environment>`.

---

## Prerequisites

- Terraform >= 1.5
- Cloud credentials with admin rights in the target account/project:
  - **AWS**: `aws configure` (or `AWS_PROFILE` / instance role) with rights
    to create VPC, ECS, RDS, ElastiCache, ALB, CloudFront, S3, Secrets
    Manager, EventBridge Scheduler, IAM, and CloudWatch resources.
  - **GCP**: `gcloud auth application-default login` and a project with
    billing enabled. The module enables the required APIs itself.
- A public **domain name** you control (required on **GCP**; optional on
  **AWS**). TLS uses ACM (AWS) / a Google-managed certificate (GCP), which
  require a real domain and a DNS record pointing at the load balancer
  before the certificate is issued. On **AWS** you can omit `domain_name`
  entirely and serve over CloudFront's default `*.cloudfront.net` domain
  (CloudFront terminates viewer TLS and reaches the ALB over HTTP within the
  VPC); on **GCP** the external HTTPS LB's managed certificate needs a
  domain, so it is required there.

---

## Quick start (AWS)

```bash
cd deploy/terraform/aws

terraform init
terraform plan  -var 'domain_name=drive.example.com'
terraform apply -var 'domain_name=drive.example.com'
```

Then:

1. Create the ACM DNS-validation records emitted in the
   `acm_certificate_validation_records` output in your DNS zone.
2. Point the apex/sub-domain at the CloudFront distribution
   (`cloudfront_domain_name` output) via a CNAME/ALIAS.
3. Sync the built frontend to the assets bucket:
   ```bash
   (cd frontend && npm run build)
   aws s3 sync frontend/dist "s3://$(terraform output -raw frontend_bucket)/"
   ```
4. Subscribe to the alarms SNS topic (`alarms_sns_topic_arn` output).

> **No custom domain?** Omit `-var 'domain_name=…'` entirely. The apply
> succeeds with no ACM cert / 443 listener and the app is served over the
> CloudFront default domain (`cloudfront_domain_name` output); skip steps 1–2.

## Quick start (GCP)

```bash
cd deploy/terraform/gcp

terraform init
terraform plan  -var 'project_id=my-project' -var 'domain_name=drive.example.com'
terraform apply -var 'project_id=my-project' -var 'domain_name=drive.example.com'
```

Then:

1. Point an `A` record for the domain at the `load_balancer_ip` output.
   The Google-managed certificate provisions automatically once DNS
   resolves (can take 15–60 min).
2. Sync the built frontend to the assets bucket:
   ```bash
   (cd frontend && npm run build)
   gsutil -m rsync -r frontend/dist "gs://$(terraform output -raw frontend_bucket)"
   ```

---

## Validation

The configs are kept `terraform fmt`-clean and pass `terraform validate`
without cloud credentials:

```bash
for d in aws gcp; do
  ( cd "deploy/terraform/$d" \
      && terraform init -backend=false \
      && terraform validate )
done
terraform fmt -recursive -check deploy/terraform
```

For deeper, provider-aware static analysis (invalid attributes, deprecated
arguments, bad instance types) each module ships a `.tflint.hcl`. Install
[`tflint`](https://github.com/terraform-linters/tflint), then:

```bash
for d in aws gcp; do
  ( cd "deploy/terraform/$d" && tflint --init && tflint )
done
```

---

## Required & notable variables

### Shared

| Variable | Default | Notes |
| --- | --- | --- |
| `domain_name` | `""` | Public domain; drives TLS. **Required on GCP**; **optional on AWS** (omit to use CloudFront's default `*.cloudfront.net` domain). |
| `environment` | `production` | Name prefix + tag/label. |
| `app_image` / `app_version` | `ghcr.io/kennguy3n/zk-drive` / `0.1.0` | Image to deploy. |
| `fabric_endpoint` / `fabric_bucket` / `fabric_console_url` | `""` | zk-object-fabric storage wiring (`S3_*`, `FABRIC_CONSOLE_URL`). |
| `stripe_secret_key` / `stripe_webhook_secret` | `""` | Optional billing secrets. |

### AWS-specific

| Variable | Default |
| --- | --- |
| `region` | `us-east-1` |
| `rds_instance_class` / `rds_replica_instance_class` | `db.t4g.medium` |
| `redis_node_type` | `cache.t4g.small` |
| `redis_transit_encryption` | `false` (set `true` for TLS / `rediss://` — see Security & compliance) |
| `server_min_count` / `server_max_count` | `2` / `10` |
| `worker_min_count` / `worker_max_count` | `1` / `6` |

### GCP-specific

| Variable | Default |
| --- | --- |
| `project_id` | _(required)_ |
| `region` | `us-central1` |
| `cloudsql_tier` | `db-custom-2-8192` |
| `redis_memory_size_gb` | `1` |
| `nats_url` / `clamav_address` | `""` (point at in-VPC NATS/ClamAV) |

> **NATS & ClamAV on GCP.** Cloud Run is request-driven and has no
> attached block storage, so the GCP module does not run NATS JetStream
> (needs a persistent volume) or ClamAV (needs a shared signature volume)
> as managed resources. Run them on GKE/Compute Engine in the same VPC and
> set `nats_url` / `clamav_address` to their in-VPC addresses. The AWS
> module runs both as ECS Fargate services (EBS for JetStream, EFS for the
> ClamAV signature DB).

---

## Architecture notes

- **Connection pooling.** AWS runs a PgBouncer sidecar in each ECS task;
  GCP runs a Cloud SQL Proxy sidecar in each Cloud Run service. The app
  always reaches Postgres at `127.0.0.1` and never holds the DB password
  itself (it reads a `DATABASE_URL` from Secrets Manager / Secret Manager).
- **Autoscaling.** The server scales on request volume (ALB request count
  per target ≈ 200 req/s per instance on AWS; Cloud Run request
  concurrency on GCP). The worker scales independently on NATS JetStream
  pending message count (a custom CloudWatch metric on AWS) with a CPU
  guardrail.
- **Secrets.** `JWT_SECRET` and `CREDENTIAL_ENCRYPTION_KEY` are generated
  by Terraform on first apply so a clean stack boots; rotate them out of
  band for real deployments. Env var names match
  `internal/config/config.go`.
- **Scheduled jobs (AWS).** `reconciler` (hourly :17), `orphan-gc` (every
  6 h :37), and `audit-archiver` (daily 03:47) run as one-shot ECS tasks
  via EventBridge Scheduler — matching the cadences in
  `deploy/k8s/*-cronjob.yaml`. On GCP these run as the worker's in-process
  loops (`RECONCILE_INTERVAL_MINUTES` / `GC_INTERVAL_MINUTES`), since the
  worker Cloud Run service is kept warm (`min_instances >= 1`,
  `cpu_idle = false`).
  - **Belt-and-suspenders / tuning (AWS).** The AWS worker still runs its
    in-process reconcile/GC loops at their defaults (60 min / 360 min)
    *alongside* the EventBridge-scheduled tasks. This is intentional and
    mirrors the Kubernetes design (`deploy/k8s/reconciler-cronjob.yaml`):
    the operations are idempotent, so the dedicated cron tasks and the
    worker loops are redundant by design. To avoid the redundant Postgres
    load and rely solely on the scheduled tasks, set
    `RECONCILE_INTERVAL_MINUTES=0` and `GC_INTERVAL_MINUTES=0` on the worker
    task (both default loops then disable themselves). Leave them at their
    defaults if you'd rather keep the safety net.
- **Redis in-transit encryption.** Both modules keep the cache on private
  subnets/VPC networks reachable only by the app, and ship with TLS to Redis
  **disabled** to avoid the per-op handshake cost. For compliance regimes
  (SOC2/HIPAA) that mandate encryption in transit even inside the VPC, set
  `redis_transit_encryption = true` on **AWS**: ElastiCache then requires TLS
  and the app connects over `rediss://` (ElastiCache presents an
  Amazon-CA-signed certificate that the stock Redis client trusts via the
  system root store — no app change needed). On **GCP**, Memorystore's
  in-transit encryption uses a Google-managed CA that the unmodified client
  cannot verify when dialing by private IP, so enabling it cleanly requires
  an app-side change (plumbing the server CA into the Redis client) and is
  intentionally left off here — see the comment in `gcp/memorystore.tf`.

---

## Rough monthly cost estimate

Order-of-magnitude only — actual spend depends on region, traffic, and
data volume. Figures are for the **default** SME-sized footprint, USD,
on-demand (no committed-use / savings plans).

### AWS (`us-east-1`)

| Component | Default | ~USD/mo |
| --- | --- | --- |
| RDS `db.t4g.medium` Multi-AZ | primary | ~$120 |
| RDS `db.t4g.medium` read replica | single-AZ | ~$60 |
| ECS Fargate — server | 2 × (1 vCPU / 2 GB) | ~$75 |
| ECS Fargate — worker | 1 × (1 vCPU / 2 GB) | ~$36 |
| ECS Fargate — NATS + ClamAV | 0.5 vCPU/1 GB + 1 vCPU/2 GB | ~$50 |
| ElastiCache `cache.t4g.small` | 2 nodes | ~$50 |
| ALB | 1 | ~$20 + LCU |
| NAT gateways | 2 AZ | ~$65 + data |
| CloudFront / S3 / Secrets / logs | low traffic | ~$15 |
| **Total** | | **~$500–580/mo** |

### GCP (`us-central1`)

| Component | Default | ~USD/mo |
| --- | --- | --- |
| Cloud SQL `db-custom-2-8192` HA | primary | ~$200 |
| Cloud SQL read replica | zonal | ~$100 |
| Cloud Run — server + worker | warm min-1 each | ~$70 |
| Memorystore Redis STANDARD_HA | 1 GB | ~$70 |
| External HTTPS LB + Cloud CDN | forwarding + egress | ~$25 |
| Serverless VPC connector | 2–3 e2-micro | ~$30 |
| Secret Manager / logging | low | ~$10 |
| **Total** | | **~$500–600/mo** |

Biggest levers: drop the read replica and/or use single-AZ Postgres
(`multi_az = false` on AWS, `availability_type = "ZONAL"` on GCP) for
non-production; both roughly halve the database line.
