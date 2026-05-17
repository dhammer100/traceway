# PVE webhook ingestion — design

**Date:** 2026-05-17
**Status:** Approved, ready for implementation plan
**Eval window:** ~2 weeks after deploy, then decide whether to keep, extend, or kill

## Goal

Accept Proxmox VE 8 notification webhooks directly into traceway and store them as log records, so backup failures, replication errors, certificate renewals, and other curated PVE alerts surface in the existing `/logs` UI alongside syslog.

This is a deliberate two-week experiment. The hypothesis: PVE's webhook `fields` map carries enough structured metadata (vmid, backup target, event type) to be more useful than syslog for curated alerts. If `fields` comes through empty or sparse, syslog wins and we delete this.

## Non-goals

- Dedicated `/alerts` page or alert-acknowledgement workflow.
- A generic `/api/webhooks/:source` framework. PVE-specific endpoint, sibling parsers later if a second source justifies it.
- A separate `pve_alerts` ClickHouse table. Reuse `log_records`.
- Webhook-specific secret management. The project bearer token is the secret.
- Retry, dedup, or idempotency on our side. PVE retries; the 30-day TTL absorbs duplicates.

## Decisions

| Question | Decision | Why |
|----------|----------|-----|
| Storage | Reuse `log_records` | 30-day TTL matches the eval window. `/logs` UI works unchanged. PVE shape fits the OTel log schema. No new migration. |
| Auth | Existing `UseClientAuth` (project bearer token) | PVE 8 webhook targets support custom headers. Zero new auth code. Per-request project scoping with no env var. |
| Endpoint | `POST /api/webhooks/pve` | YAGNI on a generic registry. Add Grafana/AlertManager parsers as siblings if/when needed. |
| Project routing | Token → project | Unlike syslog (no per-message auth, needs `SYSLOG_DEFAULT_PROJECT_ID`), the bearer token already binds to a project. |
| UI | Existing `/logs` page | Two weeks of eval doesn't justify a dedicated page. Filter by `service_name` or `pve.*` attributes. |

## Endpoint contract

```
POST /api/webhooks/pve
Authorization: Bearer <project_token>
Content-Type: application/json
```

The body shape is what the operator configures in PVE's webhook target template. We document this exact template:

```json
{
  "title": "{{ title }}",
  "message": "{{ escape message }}",
  "severity": "{{ severity }}",
  "timestamp": "{{ timestamp }}",
  "fields": {{ json fields }}
}
```

PVE 8's notification body template uses Handlebars-style `{{ }}` with `escape` and `json` built-ins. Anything else PVE renders into `fields` (event type, hostname, vmid, backup target, etc.) gets carried verbatim into `log_attributes`.

## Field mapping → `log_records`

| Source | Target column | Notes |
|--------|---------------|-------|
| `severity` (info / notice / warning / error / unknown) | `severity_text` + `severity_number` | Match the syslog importer scale: error→17, warning→13, notice→10, info→9, unknown→0 |
| `message` | `body` | Caught by existing `idx_body` tokenbf index |
| `title` | `log_attributes["pve.title"]` | Filterable as attribute |
| `timestamp` | `timestamp` | Fall back to `time.Now().UTC()` on missing/unparseable, mirroring the RFC 3164 fix |
| `fields["hostname"]` | `resource_attributes["host.name"]` | |
| `fields["type"]` (vzdump / replication / system-mail / …) | `service_name` | Lets the existing service filter slice "all PVE backup events" |
| All `fields[*]` | `log_attributes["pve.<key>"]` | Whole bag preserved for ad-hoc querying |
| Remote IP | `log_attributes["webhook.source"]` | |

## Pipeline shape

Mirror the syslog importer exactly — closest analog already in the codebase:

```
HTTP handler → parse → bounded queue → worker pool → batcher → LogRecordRepository.InsertAsync
```

The batcher flushes on either 1000 rows or 2 s, matching the existing syslog/recordings batchers. Single-row inserts into ClickHouse are the anti-pattern we're avoiding.

### Tunables

| Variable | Default | Purpose |
|----------|---------|---------|
| `WEBHOOK_WORKERS` | `4` | Parser/insert workers. `0` disables the endpoint entirely. |
| `WEBHOOK_QUEUE_SIZE` | `1024` | Bounded channel. Overflow returns 503. |
| `WEBHOOK_MAX_BODY_BYTES` | `65536` | Reject larger payloads at the handler. |

## Observability

Emitted every 10 s via `traceway.CaptureMetric`, same cadence as syslog/recordings:

| Metric | Type | Meaning |
|--------|------|---------|
| `traceway.webhooks.received` | counter (tag `source=pve`) | Accepted at HTTP layer |
| `traceway.webhooks.queue_depth` | gauge | Messages waiting on the channel |
| `traceway.webhooks.inserted` | counter | Rows written to `log_records` |
| `traceway.webhooks.dropped_overflow` | counter | Rejected because queue was full |
| `traceway.webhooks.parse_errors` | counter | Malformed JSON or missing required fields |
| `traceway.webhooks.failed` | counter | DB insert errors |

Sustained overflow or insert failures fire a rate-limited (1/min) `traceway.CaptureException`.

## Error handling

| Condition | Status | Behavior |
|-----------|--------|----------|
| Malformed JSON | 400 | Return parse error so PVE's webhook log shows it |
| Missing `severity` or `message` | 400 | Same |
| Body > `WEBHOOK_MAX_BODY_BYTES` | 413 | |
| Queue full | 503 | PVE retries |
| Auth missing/invalid | 401 (via `UseClientAuth`) | |
| DB insert failure (async) | counted, not returned | Acknowledged at HTTP layer; failures show up as metrics + CaptureException |

## Two-week evaluation criteria

After ingestion is live, the question is whether `fields` carries enough structure to be useful:

- **Win**: `fields["vmid"]`, `fields["backup-target"]`, etc. populated. `log_attributes["pve.vmid"]` filtering in `/logs` actually works. Worth keeping and possibly building richer UI.
- **Loss**: `fields` empty or only repeats what's in `message`. Syslog forwarding does the same job with no extra moving parts. Delete the endpoint.

## Out of scope, possible follow-ups

- Generic `/api/webhooks/:source` once a second source emerges.
- Severity histogram + log-pattern grouping on `/logs` (separate piece, applies to all log sources).
- Dedicated `/alerts` page with ack/silence — only if PVE notifications produce a workflow that the logs page can't model.
- Notification rules firing on log queries (e.g. "any PVE error in 5 min") — extends existing `notification_rules`.
