<h1 align="center">I just realized there is an HN post about Traceway. I didn't make it, and unfortunately my account is rate limited so I can't reply to everyone. I've written my response <a href="./HN.md">here</a></h1>

<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="Traceway%20Logo%20White.png" />
    <source media="(prefers-color-scheme: light)" srcset="Traceway%20Logo.png" />
    <img src="Traceway Logo.png" alt="Traceway Logo" width="200" />
  </picture>
</p>

<p align="center">
  <sub>Built on <img src="./docs/public/otel.png" height="14" alt="OpenTelemetry" /> <b>OpenTelemetry</b></sub>
</p>

<h3 align="center">OpenTelemetry-native observability. Open source. Self-hosted in 90 seconds.</h3>

<p align="center">
  <a href="https://opentelemetry.io"><img alt="OTel-First" src="https://img.shields.io/badge/OTel--First-Native%20OTLP%2FHTTP-425CC7?logo=opentelemetry&logoColor=white" /></a>
  <a href="https://github.com/tracewayapp/traceway/blob/main/LICENSE"><img alt="MIT License" src="https://img.shields.io/badge/100%25%20Open%20Source-MIT-22c55e" /></a>
  <a href="https://docs.tracewayapp.com/server/docker-compose"><img alt="Self-Host" src="https://img.shields.io/badge/Self--Host-docker%20compose%20up-2496ed?logo=docker&logoColor=white" /></a>
  <a href="https://discord.gg/9tPn2SB3"><img alt="Join us on Discord" src="https://img.shields.io/badge/Discord-Join%20the%20community-5865F2?logo=discord&logoColor=white" /></a>
</p>

<p align="center">
  <a href="https://tracewayapp.com">Website</a> · <a href="https://docs.tracewayapp.com">Docs</a> · <a href="https://cloud.tracewayapp.com">Cloud</a> · <a href="https://discord.gg/9tPn2SB3">Discord</a>
</p>

---

Traceway is an **OpenTelemetry-native** observability platform that combines **logs, traces, metrics, session replay/RUM, exceptions, and AI tracing** together. Point an OTLP exporter at it and you're in business. No Collector, no glue code, no per-language vendor SDK.

**MIT licensed. No BSL. No "open core."** Every feature is in the box. Self-host it for free, or run it on [Traceway Cloud](https://cloud.tracewayapp.com) if you'd rather not babysit infra.

<img alt="Traceway Dashboard" src="./website/public/images/performance-endpoints-impact-table.png" />

<p align="center">
  <a href="https://discord.gg/9tPn2SB3"><b>👋 Join the Traceway Community on Discord →</b></a><br>
  <sub>Chat with the team, shape the roadmap, get help, and meet other folks running Traceway in production.</sub>
</p>

## Fork notes (`dhammer100/traceway`)

This fork of [`tracewayapp/traceway`](https://github.com/tracewayapp/traceway) replaces the public HTTP listener with a Tailscale [`tsnet`](https://tailscale.com/kb/1244/tsnet) node so the backend (SDK ingestion + dashboard + static SPA) is reachable only over a tailnet, and lands a batch of backend security fixes from an audit of `backend/`. All changes live on the `security-and-tsnet` branch.

> Frontend / Go SDK / docs were intentionally **out of scope** for this fork.

### tsnet listener (tailnet-only)

Env mode now calls `tsnet.Server.Listen` (or `ListenTLS` if `TSNET_HTTPS=true`) instead of `router.Run(:port)`. There is no public listener — SDKs and browsers must reach Traceway over the tailnet. Embedded mode (`tracewaybackend.Run(WithPort(…))`) still uses stdlib HTTP so tests / `examples/embedded-backend-otel` don't need a tailnet.

| Env var | Required | Default | Purpose |
|---|---|---|---|
| `TSNET_HOSTNAME` | yes | — | Node name registered with the tailnet (e.g. `traceway`) |
| `TSNET_AUTHKEY` | first start | — | Tailscale auth key; can be omitted after `TSNET_DIR` is persisted |
| `TSNET_DIR` | no | `./tsnet-state` | Directory where tsnet persists the node identity. **Mount as a volume.** |
| `TSNET_LISTEN_ADDR` | no | `:80` (or `:443` if HTTPS) | Listen address inside the tailnet |
| `TSNET_HTTPS` | no | `false` | If `true`, use `srv.ListenTLS` (LetsEncrypt via Tailscale MagicDNS) |
| `TSNET_LOGF` | no | `quiet` | Set anything other than `quiet` to enable tsnet's internal logger |

`PORTS` is ignored (and logged) when tsnet mode is active. `APP_BASE_URL` should point at the tailnet hostname (`http://traceway/`) — invitation and password-reset emails use it.

### Backend security fixes

Grouped by severity from the audit report. Every item below has been applied on this branch.

#### Critical
- **C1** — Hardcoded JWT secret in embedded mode removed; new `WithJWTSecret(...)` option, ephemeral random fallback for tests so embedded users never accidentally ship the public dev key.
- **C2** — `/api/report` gzip body capped: 32 MB raw + 256 MB decompressed. Project tokens ship to browsers via the JS SDK, so the prior unbounded gzip path was a public DoS.

#### High
- **H1 + M6** — Source-map uploads sanitize `version` and filename to `[A-Za-z0-9._-]{1,128}`, `filepath.Base()` on Filename, defense-in-depth containment check in `storage/local.go`. Prior code wrote `filepath.Join(basePath, "../../...")` to disk.
- **H2** — OAuth no longer auto-links to an existing local account on email match; redirects with `email_in_use_signin_with_existing_method` so a provider returning an unverified email cannot take over a local-password account.
- **H3** — Password-reset + invitation tokens are now `crypto/rand` 32-byte values, SHA-256 hashed before DB storage; raw token only in the email. A DB leak can no longer replay outstanding tokens.
- **H4** — SMTP now does `STARTTLS` (ports 587/25) or implicit TLS (465); refuses to send credentials if STARTTLS isn't available.
- **H5** — Email header values reject CR/LF; inviter name / org name / recipient sanitized before interpolation. Prior code allowed an attacker to inject `Bcc:` via their own user name and exfiltrate invitation tokens.
- **H6** — All email lookups case-insensitive (`FindByEmail`, `IsUserMemberByEmail`, invitation lookups); emails lowercased on insert; `AcceptExistingUser` uses `strings.EqualFold`.

#### Medium
- **M1 + L1** — `/login` and `/forgot-password` burn a constant-time bcrypt budget on the missing-user branch via `services.DummyTimingWork` so response time no longer enumerates registered emails.
- **M2** — Global `MaxBody` middleware caps non-telemetry JSON to 1 MB, `/api/sourcemaps/upload` to 50 MB. `/api/report` and `/api/otel/v1/*` keep their own larger telemetry caps.
- **M3** — `OAUTH_SESSION_SECRET` is required and must be ≥ 32 chars when an OAuth provider is configured. HMAC-derived auth + encryption keys (cookies are now encrypted, not just signed) — no more re-using `JWT_SECRET` across primitives.
- **M4** — Member-role updates re-validate the allowed set (`admin|user|readonly`) in the controller body *and* in `OrganizationRepository.UpdateUserRole`; `owner` is rejected.
- **M5 + L2** — JWT carries a `tv` (tokenVersion) claim; `UseAppAuth` verifies it against the live `users.token_version` column on every request; bumped on every `UpdatePassword`. JWT TTL cut from 7d → 24h. New migrations: `pg/0035_add_token_version_to_users.up.sql`, `sqlite/0010_add_token_version_to_users.up.sql`.
- **M7** — `ProjectCache` keys tokens by SHA-256 hash instead of the raw value, removing the map-lookup timing oracle.
- **M9** — Monitoring `tracewaygin` forwarder no longer captures `RecordingBody` or `RecordingHeader`. The upstream Traceway no longer receives `Authorization` bearer tokens or login / reset-password request bodies on error.

#### Low / Info
- **L3** — `UpdatePassword` switched to `lit.UpdateNative` with an explicit `UPDATE users SET password = …, token_version = token_version + 1 WHERE id = …` so other columns can't be clobbered with zero values.
- **L4** — `/api/report` bind error no longer echoes `err.Error()` to anonymous clients. Internal struct-field paths stay server-side.
- **L5** — `ProjectCache.AddProject` deferred to post-commit via new `middleware.AfterCommit` hook. A rolled-back registration can no longer leave a usable token in the in-memory cache.
- **I2** — `POSTGRES_SSLMODE` default flipped from `disable` to `require`. Local dev needs an explicit opt-out.
- **I3** — Explicit `..` / NUL reject in the SPA static handler. `embed.FS` already enforces this; the explicit check guards against a future swap to `os.DirFS`.

### Open points (not done yet)

- **I1** — `modernc.org/sqlite v1.18.1` is 3+ years stale. Needs a separate bump + `govulncheck` pass.
- **Source-map token lifecycle** — token sanitization is done, but the static per-project token still never expires and has no per-project upload rate limit. Worth a follow-up.
- **L4 broader sweep** — only `/api/report` was tightened. Authenticated dashboard endpoints still echo Gin bind errors verbatim (struct field paths + validator tags). Mild info disclosure for an already-authenticated user.
- **CORS `*` on `/api/report`** (M8 from the audit) — intentionally left as-is. Required for browser SDKs and now safer thanks to C2 + M2 caps.
- **Frontend / Go SDK / docs** — entirely out of scope for this fork. The frontend still stores the JWT in `localStorage`; M5's tokenVersion + 24h TTL is the partial mitigation.
- **Source-map upload UX** — sanitization is strict (`[A-Za-z0-9._-]`). Build tools that emit filenames with spaces or non-ASCII will now fail closed. Treat as a contract change for any CI that uploads source maps.

### Status

```
go build ./...      # clean
go vet ./...        # clean
go test ./app/services/... ./app/retention/... ./app/recordings/... # pass
```

`app/repositories` tests fail on this branch — but they also fail on `main` (pre-existing schema mismatch on `span_id` / `session_id` / `parent_span_id` columns). Not introduced by this fork.

## What's in the box

- **Logs** — Structured, trace-linked, sub-second search. Native OTLP/HTTP ingest from any OTel SDK.
- **Traces** — End-to-end span waterfalls across every service. Click a log, jump to its span.
- **Metrics** — Host, runtime, and custom metrics. Any dimension, any chart, with custom widget groups.
- **Exceptions** — SHA-256 normalized stack traces grouped into ranked issues. Source-mapped (webpack, esbuild, Vite).
- **Session Replay** — Watch what the user did right before the error. Available for web (any JS framework) and Flutter.
- **AI Observability** — LLM cost, tokens, latency, and full conversations across providers (OpenRouter and any OTel-compatible AI gateway).

Plus: configurable alerts (Slack / GitHub / email / webhook), Apdex + Impact-Score endpoint ranking, multi-tenant orgs with role-based access, and a per-endpoint slow-threshold override.

## Why Traceway

|                          | Enterprise (Datadog / New Relic) | DIY OSS stack (Prometheus + Loki + Tempo + ...) | **Traceway**                      |
| ------------------------ | -------------------------------- | ----------------------------------------------- | --------------------------------- |
| **Pricing**              | Per-event, per-host, per-seat    | Free + ops time                                 | Self-host free, fixed cloud tiers |
| **Setup**                | Vendor SDK per language          | Glue 6 tools together                           | `docker compose up -d`            |
| **License**              | Proprietary                      | Mixed (some BSL / open-core)                    | **MIT — no asterisks**            |
| **OTel**                 | Wrapped in vendor SDK            | OTel Collector required                         | **Native OTLP/HTTP ingest**       |
| **Replay + traces + AI** | 3 separate products              | Wire it yourself                                | One system, one trace ID          |

## Quick Start

### Self-host with Docker (recommended)

```bash
git clone https://github.com/tracewayapp/traceway
cd traceway && docker compose up -d
# ✓ dashboard at http://localhost
```

Point any OTel SDK at `http://localhost/api/otel/v1/traces` (or `/metrics`, `/logs`) and traces start flowing. See the [self-hosting docs](https://docs.tracewayapp.com/server/docker-compose) for production deployment, TLS, and storage configuration.

**Docker images are cryptographically signed.** See [DOCKER_SIGNATURES.md](./DOCKER_SIGNATURES.md) to verify images before deploying.

### Embedded mode (inside your Go app)

Run Traceway inside your Go process — no Docker, no external databases, SQLite under the hood:

```bash
go get github.com/tracewayapp/traceway/backend
```

```go
import tracewaybackend "github.com/tracewayapp/traceway/backend"

func main() {
    go tracewaybackend.Run(
        tracewaybackend.WithPort(8082),
        tracewaybackend.WithDefaultUser("admin@localhost.com", "admin"),
        tracewaybackend.WithDefaultProject("My App", "go", "dev-token"),
    )

    // ... start your app, point its OTel exporter to http://localhost:8082/api/otel/v1/traces
}
```

Open `http://localhost:8082`, log in, and hit your app to see traces appear. Full walkthrough in the [embedded mode guide](https://docs.tracewayapp.com/learn/embedded-mode), or check the [working example](./examples/embedded-backend-otel).

## Supported Integrations

Traceway integrates with the tools you already use. Every integration ships traces, metrics, and logs over **OTLP/HTTP** — no proprietary SDK required.

> View the full list in the [documentation](https://docs.tracewayapp.com/client). Missing a framework? [Open an issue](https://github.com/tracewayapp/traceway/issues) to request it.

### Backend

<table width="100%">
<tbody>
<tr>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/gin-middleware"><img src="./docs/public/gin.png" height="28" alt="Gin" /><br/><b>Gin</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/chi-middleware"><img src="./docs/public/chi.png" height="28" alt="Chi" /><br/><b>Chi</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/fiber-middleware"><img src="./docs/public/fiber.svg" height="28" alt="Fiber" /><br/><b>Fiber</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/fasthttp-middleware"><img src="./docs/public/fasthttp.png" height="28" alt="FastHTTP" /><br/><b>FastHTTP</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/http-middleware"><img src="./docs/public/stdlib.png" height="28" alt="net/http" /><br/><b>net/http</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/sdk"><img src="./docs/public/custom.png" height="28" alt="Go Generic" /><br/><b>Go Generic</b></a></td>
</tr>
<tr>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/node-sdk"><img src="./docs/public/node.png" height="28" alt="Node.js" /><br/><b>Node.js</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/nestjs"><img src="./docs/public/nestjs.png" height="28" alt="NestJS" /><br/><b>NestJS</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/hono"><img src="./docs/public/hono.png" height="28" alt="Hono" /><br/><b>Hono</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/symfony"><img src="./docs/public/symfony.png" height="28" alt="Symfony" /><br/><b>Symfony</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/cloudflare"><img src="./docs/public/cloudflare.png" height="28" alt="Cloudflare Workers" /><br/><b>Cloudflare</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/otel"><img src="./docs/public/otel.png" height="28" alt="OpenTelemetry" /><br/><b>OpenTelemetry</b></a></td>
</tr>
</tbody>
</table>

### Frontend

> Session Replay is included with every frontend integration — and with Flutter too.

<table width="100%">
<tbody>
<tr>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/nextjs"><img src="./docs/public/nextjs.png" height="28" alt="Next.js" /><br/><b>Next.js</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/react"><img src="./docs/public/react.png" height="28" alt="React" /><br/><b>React</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/vue"><img src="./docs/public/vue.png" height="28" alt="Vue" /><br/><b>Vue</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/svelte"><img src="./docs/public/svelte.png" height="28" alt="Svelte" /><br/><b>Svelte</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/jquery"><img src="./docs/public/jquery.png" height="28" alt="jQuery" /><br/><b>jQuery</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/js-sdk"><img src="./docs/public/javascript.png" height="28" alt="JavaScript" /><br/><b>JavaScript</b></a></td>
</tr>
</tbody>
</table>

### Mobile

<table>
<tbody>
<tr>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/flutter"><img src="./docs/public/flutter.png" height="28" alt="Flutter" /><br/><b>Flutter</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/android"><img src="./docs/public/android.png" height="28" alt="Android" /><br/><b>Android</b></a></td>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/react-native"><img src="./docs/public/react.png" height="28" alt="React Native" /><br/><b>React Native</b></a></td>
</tr>
</tbody>
</table>

### AI

<table>
<tbody>
<tr>
<td align="center" width="150"><a href="https://docs.tracewayapp.com/client/openrouter"><img src="./docs/public/openrouter.png" height="28" alt="OpenRouter" style="filter: contrast(0.5);" /><br/><b>OpenRouter</b></a></td>
</tr>
</tbody>
</table>

## Screenshots

<table width="100%">
<tr>
<td width="50%" valign="top"><b>Logs — trace-linked search</b><br><img src="./website/public/images/logs-search-and-detail.png" alt="Logs" /></td>
<td width="50%" valign="top"><b>Span waterfall</b><br><img src="./website/public/images/traces-spans-waterfall.png" alt="Spans" /></td>
</tr>
<tr>
<td width="50%" valign="top"><b>Metrics — application dashboard</b><br><img src="./website/public/images/metrics-application-dashboard.png" alt="Metrics" /></td>
<td width="50%" valign="top"><b>Exceptions — grouped & ranked</b><br><img src="./website/public/images/exceptions-grouped-ranked.png" alt="Exceptions" /></td>
</tr>
</table>

## Tech Stack

| Component     | Technology                                            |
| ------------- | ----------------------------------------------------- |
| Backend       | Go 1.25, Gin                                          |
| Frontend      | SvelteKit 2, Svelte 5, Tailwind CSS v4                |
| Telemetry DB  | ClickHouse (standalone) or SQLite (embedded)          |
| Relational DB | PostgreSQL (standalone) or SQLite (embedded)          |
| Ingest        | OTLP/HTTP (Protobuf + JSON) for traces, metrics, logs |

## Project Structure

| Directory   | Description                                                                                                                                                                                                                             |
| ----------- | --------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `backend/`  | Go/Gin API server — OTLP ingest, REST API, notifications, migrations                                                                                                                                                                    |
| `frontend/` | SvelteKit 2 dashboard SPA                                                                                                                                                                                                               |
| `docs/`     | Documentation site (Nextra)                                                                                                                                                                                                             |
| `examples/` | Working examples — [embedded mode](./examples/embedded-backend-otel) and OTel-instrumented apps ([Express](./examples/express-otel), [NestJS](./examples/nestjs-otel), [Next.js](./examples/nextjs-otel), [Hono](./examples/hono-otel)) |
| `website/`  | Landing page                                                                                                                                                                                                                            |

## Build Tags

| Tag         | Purpose                                                                                                         |
| ----------- | --------------------------------------------------------------------------------------------------------------- |
| _(none)_    | SQLite storage — embedded mode, zero dependencies. This is the default.                                         |
| `pgch`      | ClickHouse + PostgreSQL storage — standalone server mode.                                                       |
| `localdist` | Embeds frontend from `static/dist/` instead of `static/frontend/`. Used by traceway-cloud to inject billing UI. |

```bash
# Embedded mode (SQLite, default)
cd backend && go build ./cmd/traceway

# Standalone server (ClickHouse + PostgreSQL)
cd backend && go build -tags pgch ./cmd/traceway
```

## Running Tests

```bash
# SQLite tests (default, no tags needed)
cd backend && go test -v -count=1 ./app/repositories/

# ClickHouse + PostgreSQL tests (requires Docker)
./scripts/test-backend-pgch.sh

# OTEL trace converter tests (no DB required)
cd backend && go test -v -count=1 ./app/controllers/otelcontrollers/

# Update OTEL golden files after intentional converter changes
cd backend && go test -v -count=1 -args -update ./app/controllers/otelcontrollers/
```

## Documentation

Full documentation at **[docs.tracewayapp.com](https://docs.tracewayapp.com)**:

- [**Client SDKs**](https://docs.tracewayapp.com/client) — OpenTelemetry, Go, Node.js, Python, and more
- [**Self-Hosting**](https://docs.tracewayapp.com/server) — Docker Compose and production deployment
- [**Concepts**](https://docs.tracewayapp.com/learn) — How tracing, exception grouping, metrics, and alerts work
- [**Embedded Mode**](https://docs.tracewayapp.com/learn/embedded-mode) — Run Traceway inside your Go app

## Community

Traceway is built in the open, and the **[Discord community](https://discord.gg/9tPn2SB3)** is where it happens. Come say hi — whether you're kicking the tires, running it in production, or just curious. We use it to:

- 🗣️ **Talk through ideas** — feature requests, integration asks, roadmap input
- 🛟 **Help each other out** — setup, OTel wiring, deployment questions
- 🚀 **Show & tell** — share what you're building and how you're using Traceway
- 🐛 **Catch bugs early** — report issues and get fast feedback from maintainers
- 👀 **Get the inside scoop** — sneak peeks at what's shipping next

<p>
  <a href="https://discord.gg/9tPn2SB3"><img alt="Join the Traceway Community on Discord" src="https://img.shields.io/badge/Join%20the%20Community-on%20Discord-5865F2?style=for-the-badge&logo=discord&logoColor=white" /></a>
</p>

## Contribute

Contributions are welcome — pull requests get reviewed and merged. If you're not sure where to start or want to discuss an idea first, [open an issue](https://github.com/tracewayapp/traceway/issues) or drop by the [community Discord](https://discord.gg/9tPn2SB3) and we'll talk it through.

## Links

- [Website](https://tracewayapp.com)
- [Documentation](https://docs.tracewayapp.com)
- [Traceway Cloud](https://cloud.tracewayapp.com) — managed hosting (same MIT code, run by us)
- [Community Discord](https://discord.gg/9tPn2SB3) — chat with the team and other users
