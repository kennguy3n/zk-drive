# 6. Operations without an ops team

**Persona:** Admin / IT lead (the same person who onboarded the team — and who
also does everything else)
**Job to be done:** *"Tell me, at a glance, whether the system is healthy — and
don't make me run a query or read a log to find out."*

---

SMEs do not have a 24/7 SRE rotation. ZK Drive's operational story is built
around that reality: the things that would normally require an ops team should
either run themselves or surface their status plainly.

## One screen that tells the truth

The **Admin → Health** tab is a live status board for every subsystem. Green is
healthy, yellow is degraded, red needs attention, grey is "not configured." It
auto-refreshes. This is the real board from the demo deployment:

![System health dashboard](img/24-admin-health.png)

Reading it honestly, top to bottom:

- **Postgres — Healthy.** With live connection-pool stats (1 acquired, 4 idle,
  5 total, max 20). The metadata store is up and not saturated.
- **NATS — Healthy.** Connected, stream `DRIVE_JOBS`, 0 pending messages — the
  async job backlog has been drained.
- **Worker — Healthy.** All five worker types reporting green with recent
  heartbeats: `archive`, `classify`, `index`, `preview`, `scan`.
- **ClamAV / Redis / OnlyOffice — Not configured.** Honestly greyed out: these
  optional subsystems were not provisioned in this minimal demo.
- **Storage — Unhealthy ("gateway unreachable").** The dashboard is doing its
  job: the zk-object-fabric storage *control plane* is not provisioned here, so
  the probe correctly reports a problem rather than pretending everything is
  fine.

That last point is the most important one for trust: **the dashboard surfaces
real failures.** A status board that is always green is useless. This one told
us, accurately, what we had and had not wired up.

## Why "NoOps" is a design goal, not a slogan

- **Self-draining pipelines.** Uploads queue preview/index/scan/classify work on
  NATS JetStream; workers pick it up and the queue returns to zero on its own —
  no cron, no babysitting. You can watch it happen: `DRIVE_JOBS` shows 0 pending
  messages after the worker came online.
- **Connection-pool visibility.** Postgres pool stats are on the same screen, so
  capacity problems are visible before they become outages.
- **Honest degradation.** Optional components degrade to "not configured" or
  surface as unhealthy — the system keeps serving what it can and tells you
  what it cannot.

## Where files physically live

For customers who care about data residency, the **Placement policy** screen
exposes provider, region, country (ISO-3166), and storage class — the controls
that decide which jurisdiction your bytes sit in.

![Placement policy controls](img/26-admin-placement.png)

> **Honest caveat.** This screen shows *"This operation is not supported"*
> because, again, the zk-object-fabric storage control plane is not provisioned
> in the demo. The residency controls are real product surface; exercising them
> requires the storage backend, which we deliberately did not stand up here.

---

### What this journey demonstrates

- **At-a-glance health** for every subsystem, auto-refreshing, with real
  connection and queue metrics.
- **A dashboard that tells the truth** — it flagged exactly what was and wasn't
  configured in our demo instead of papering over it.
- **Self-managing async pipelines** that drain themselves with no operator.
- **Data-residency controls** for customers who need to pin jurisdiction.

Next: [Honest assessment vs the competition →](07-honest-competitive-assessment.md)
