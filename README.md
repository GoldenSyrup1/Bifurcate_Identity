# Bifurcate-Identity

A small Go service demonstrating the one piece of a CIAM migration everyone
skips in a portfolio project: what happens when a write dies **between**
the legacy identity store and the new one, mid-migration.

Built for the SafetyCulture (Mitti) CIAM Software Engineer II application —
the JD's own line is "contribute to safe, incremental migration off legacy
identity services using feature flags, backwards compatibility and staged
rollout." This is that mechanism, runnable.

## What it does

Each tenant moves through four feature-flagged phases, one step at a time:

1. **legacy_only** — everything hits the old store.
2. **dual_write** — writes go to both stores; reads still come from legacy,
   so nothing client-visible changes yet.
3. **shadow_verify** — reads still come from legacy, but every read is also
   checked against the new store and any disagreement is logged — catching
   drift before it's load-bearing, not after.
4. **new_only** — cutover. Legacy is kept around, not deleted, as a
   rollback path.

The dashboard lets you:
- Create tenants and step them through the phases
- Sign up / log in test users and watch which backend answers
- **Arm chaos** — the next signup during dual_write/shadow_verify will
  write to legacy, then simulate the process dying before the write to the
  new store happens. The live log shows exactly that moment.
- **Run reconciliation** — scans both stores, finds the gap the crash left,
  and heals it by replaying the missing write. Watch the "modern" count
  catch up to "legacy" in real time.
- Retry the same signup (same request_id) and see it get deduped instead
  of creating a duplicate user — the idempotency half of "retries are safe."

## Run it locally

```
go run .
```

Then open http://localhost:8080

## Deploy to Railway

Push this folder to its own GitHub repo, then in Railway: New Project →
Deploy from GitHub repo. `railway.json` and `Procfile` are already set up
for Nixpacks to detect and build the Go module automatically. No database
or env vars needed — everything is in-memory by design, so a restart just
resets the demo state.

## Why in-memory, not Postgres

The point of this project is the migration *mechanism* — feature-flag
phases, dual-write, drift detection, chaos-tested reconciliation — not
database plumbing. The `store.Store` interface is the seam: swapping either
backend for real Postgres tables is a constructor change, not an
architecture change.
