<p align="center">
  <picture>
    <source media="(prefers-color-scheme: dark)" srcset="./assets/ranking-logo-dark.svg" />
    <source media="(prefers-color-scheme: light)" srcset="./assets/ranking-logo-light.svg" />
    <img src="./assets/ranking-logo-light.svg" alt="CPA Usage Ranking" width="520" />
  </picture>
</p>

<p align="center">
  <a href="./README.md"><strong>English</strong></a> ｜ <a href="./README.zh.md">简体中文</a>
</p>

<h1 align="center">CPA Usage Ranking</h1>

<p align="center">Open-source AI usage leaderboard server and web dashboard for the CLIProxyAPI (CPA) ecosystem.</p>

<p align="center">
  <strong><a href="#client-integration">Client Integration</a>&emsp;|&emsp;<a href="#quick-start">Quick Start</a>&emsp;|&emsp;<a href="#leaderboards--scoring">Leaderboards &amp; Scoring</a>&emsp;|&emsp;<a href="#admin-console">Admin Console</a>&emsp;|&emsp;<a href="#configuration">Configuration</a></strong>
</p>

---

**CPA Usage Ranking** is a standalone, self-hosted leaderboard server built for downstream projects in the **[CLIProxyAPI (CPA)](https://github.com/router-for-me/CLIProxyAPI)** ecosystem—including **cpa-usage-keeper (Keeper)**, **cpamp**, **oh-my-cpa**, and other CPA dashboards, usage collectors, or custom integrations.

The server is decoupled from any specific client codebase, SDK, or upstream event table. Clients integrate over a compact HTTP JSON protocol to register an identity, push **UTC daily cumulative usage snapshots**, and read cached public rankings—without ever sending upstream API keys, prompts, or response payloads.

## Highlights

- **Open Client Protocol**: Clients generate and hold their own 32-byte random secret (`base64url` Bearer); the server stores only its `SHA-256` digest. GitHub authorization pairs a numeric account ID and CPA UUID into one seat that can have multiple writers.
- **Privacy-Minimal & Idempotent**: Accepts only aggregated per-model token counts, request counts, success counts, and mean latencies. Writer-specific `revision` numbers make retries safe; only the last committed replacement counts for the seat/day.
- **Five Boards & Complete UTC Periods**: Ships **Overall, Usage, Cache Reuse, Reliability (Wilson), and TTFT** leaderboards across **Today, Yesterday, Weekly (previous complete UTC week), and Monthly (previous complete UTC month)** views, preserving exact 64-bit integer precision end-to-end.
- **Lightweight High-Throughput SQLite Engine**: Built with Go and SQLite (`WAL`, `FULL` sync, serialized write queue), featuring automatic window closure, background settlement/retention, and atomic in-memory cache snapshots—a historical build reached [500 sustained report TPS](docs/benchmark.md) under a 2 vCPU / 1 GiB limit; this is not a current protocol capacity claim.
- **Built-in Web UI & Optional Admin Console**: Serves a fast, minimal responsive leaderboard UI (desktop, tablet, and mobile, with 66 built-in WebP avatars) on the same origin, plus an opt-in administrator console for user search, bans, and independent leaderboard exclusion.

## Client Integration

Designed for `keeper`, `cpamp`, `oh-my-cpa`, and any collector or dashboard that reports usage or displays standings. For full field rules, retry state machines, and error codes, see the **[Client Integration Guide (中文)](docs/integration.md)** and **[Integration Sequence & Flow Diagrams (中文)](docs/integration-flow.md)**.

### Protocol Overview

| Step | Endpoint | Auth | Purpose |
| --- | --- | --- | --- |
| 1. Generate secret | Local to client | — | Generate 32 CSPRNG bytes, encode as 43-char unpadded `base64url`, and persist locally |
| 2. Start registration | `POST /v1/clients/register/start` | `Bearer <secret>` | Send the persistent CPA UUID and `return_to`; open the returned authorization URL in the browser |
| 3. Complete registration | `POST /v1/clients/register/complete` | Same Bearer | Send `request_id`; `201` creates a seat, `200` reuses or recovers one, `202` awaits authorization |
| 4. Push daily snapshot | `PUT /v1/clients/report/{utc_day}` | `Bearer <secret>` | UTC **today and yesterday**; revisions are per writer, the full snapshot replaces seat/day data |
| 5. Withdraw (optional) | `POST /v1/clients/withdraw` | `Bearer <secret>` | No body; `204` means the whole seat is withdrawn and all writers stop |
| Read leaderboards | `GET /v1/leaderboards/{board}/{period}` | None | Fetch the latest published top-100 cache |

> **Aggregation rule**: Clients merge daily usage into one item per canonical model before uploading (no `service_tier` field is accepted). Weight `ttft_avg_ms` and `latency_avg_ms` by their respective sample counts (`null` with `"0"` samples when unmeasured). Unrecognized model names are accepted and ignored from totals; matched [canonical models and aliases](docs/model-catalog.md) feed the rankings.

### Minimal cURL Example

Use an [OAuth-configured Ranking HTTPS deployment](docs/docker.md#github-oauth-app). The CPA UUID here is illustrative; use the persistent UUID supplied by your CPA instance and your desired browser return page.

```sh
# 1. Generate and persist a 43-char unpadded base64url secret (32 random bytes)
SECRET=$(openssl rand -base64 32 | tr '+/' '-_' | tr -d '=\n')

# 2. Save the returned request_id, and open authorize_url in a browser
BASE=https://ranking.example.com
curl -sS -X POST "$BASE/v1/clients/register/start" \
  -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
  -d '{"cpa_uuid":"550e8400-e29b-41d4-a716-446655440000","return_to":"http://localhost:3001/settings?tab=ranking#status"}'

# 3. After GitHub authorization, complete with the original ID and secret
REQUEST_ID='replace with returned request_id'
curl -sS -X POST "$BASE/v1/clients/register/complete" \
  -H "Authorization: Bearer $SECRET" -H 'Content-Type: application/json' \
  -d "{\"request_id\":\"$REQUEST_ID\"}"

# 4. After 200 or 201, upload the full cumulative snapshot for the UTC day
TODAY=$(date -u +%F)
curl -sS -X PUT "$BASE/v1/clients/report/$TODAY" \
  -H "Authorization: Bearer $SECRET" \
  -H "Content-Type: application/json" \
  -d '{
    "schema_version": 1,
    "revision": "1",
    "items": [{
      "model": "claude-opus-4-5",
      "total_tokens": "150000",
      "input_tokens": "100000",
      "output_tokens": "50000",
      "cache_read_tokens": "60000",
      "cache_write_tokens": "0",
      "reasoning_tokens": "10000",
      "request_count": "42",
      "success_count": "41",
      "ttft_avg_ms": 320.5,
      "ttft_samples": "41",
      "latency_avg_ms": 1850.0,
      "latency_samples": "41"
    }]
  }'

# Read the public Overall leaderboard without a credential
curl -sS "$BASE/v1/leaderboards/overall/today"
```

## Leaderboards & Scoring

`GET /v1/leaderboards/{board}/{period}` serves prebuilt snapshots from the atomic in-memory cache. Full formulas, tie-breaking, and precision rules are documented in **[Leaderboard API & Scoring](docs/leaderboards.md)**.

### Boards (`board`)

| Key | Board | Metric & Ranking Rule |
| --- | --- | --- |
| `overall` | Overall | 100-point composite score: usage mid-rank percentile (40) + 95% Wilson success lower bound (30) + cache reuse rate (20) + TTFT vs. positive median (10), plus 6 detail metrics |
| `usage` | Token Usage | Matched `total_tokens` sorted as exact 64-bit integers |
| `cache` | Cache Reuse | `cache_read_tokens / input_tokens` percentage (requires `input_tokens > 0`) |
| `success` | Reliability | 95% Wilson lower-bound score balancing high success rate with request volume |
| `ttft` | First-Token Latency | Mean time-to-first-token (`ttft_mean_ms`) sorted ascending (lower is better) |

### Periods (`period`)

| Key | UI Label | UTC Range (half-open `[start, end)`) |
| --- | --- | --- |
| `today` | Today (`今日`) | Current UTC calendar day `[today 00:00, tomorrow 00:00)` |
| `yesterday` | Yesterday (`昨日`) | Previous UTC calendar day `[yesterday 00:00, today 00:00)` |
| `week` | Weekly (`周榜`) | **Previous complete UTC Monday–Monday week** `[last Mon 00:00, this Mon 00:00)` |
| `month` | Monthly (`月榜`) | **Previous complete UTC calendar month** `[1st of last month, 1st of this month)` |

> The backend cache also maintains `year` (current UTC year) and `all` (all-time) views via the API; the web UI focuses on the four primary periods above.

## Quick Start

Docker Compose is recommended (`linux/amd64` and `linux/arm64`). The container bundles the static Go/SQLite binary, built web assets, embedded avatars, and model catalog—no host Go or Node.js toolchain is needed. See the **[Docker Deployment Guide](docs/docker.md)** for full details.

### 1. Build and run locally with Compose

```sh
docker compose -p cpa-ranking up -d --build
docker compose -p cpa-ranking ps
```

### 2. Run a published GHCR image

Published images use immutable `vMAJOR.MINOR.PATCH` and `sha-<commit>` tags (no mutable `latest` tag):

```sh
export RANKING_IMAGE=ghcr.io/<owner>/cpa-usage-ranking:v0.1.0
docker compose -p cpa-ranking pull ranking
docker compose -p cpa-ranking up -d --no-build
```

### Production Reverse-Proxy Notes

- **Enable GitHub registration**: Create a GitHub OAuth App with `/auth/github/callback` on the public HTTPS origin and supply the app settings at runtime. With none configured, new registrations stay closed; existing writers and public reads still work. See [GitHub OAuth App](docs/docker.md#github-oauth-app).

- **Terminate HTTPS in front of the service**: Registration and reporting send Bearer secrets. `compose.yaml` binds loopback `127.0.0.1:8080:8080` by default; place Nginx, Caddy, or Traefik in front to serve TLS before accepting remote traffic. Omit credentials, cookies and complete OAuth query strings from logs.
- **Configure trusted proxies**: Forwarded IP headers are ignored by default. Set `RANKING_TRUSTED_PROXIES` to your reverse proxy IPs/CIDRs so per-source rate limits see real client addresses (see [Registration Sources](docs/registration-sources.md)).
- **Single instance on local storage**: The container runs as non-root `10001:10001` with SQLite persisted in `/data` (`cpa-ranking_ranking-data`). Run a single instance on local disk—do not share SQLite across replicas or NFS.

## Admin Console

The administrator console is disabled by default; public leaderboards and client ingestion work without it.

To enable moderation and administrator management, create a private JSON file outside `RANKING_WEB_DIR` with `0600` or `0640` permissions (readable by UID/GID `10001` in Docker), set `RANKING_ADMIN_CONFIG_FILE`, and restart:

```json
{
  "username": "admin",
  "password": "replace-with-strong-password-12-to-72-bytes"
}
```

Once enabled, the header exposes **Sign in / Console** (`/admin/leaderboards`), supporting:
- **Leaderboard Moderation**: Inspect boards, trigger immediate cache refreshes, and **exclude / restore** participants (`ranking_hidden` removes a client from all 5 boards, 6 periods, and score references while still accepting their daily reports).
- **User Management**: Search registered clients by literal display-name substring or exact ID, and **ban / unban** (`disabled` rejects future reports and removes the client from all boards; ban and exclusion flags remain independent).
- **Administrator Accounts** (owner only): Create or delete ordinary database administrators with immediate session revocation.

See **[Admin Console (中文)](docs/admin.md)** for full security and Docker mount examples.

## Configuration

Core startup and admin settings are listed below (both source defaults and [Docker image overrides](docs/docker.md#configuration-and-health) are shown; empty or malformed values fail fast at startup):

| Variable | Source Default (Docker Override) | Description |
| --- | --- | --- |
| `RANKING_HTTP_ADDR` | `127.0.0.1:8080` (`0.0.0.0:8080`) | HTTP listen address (`host:port`) |
| `RANKING_SQLITE_PATH` | `data/ranking.db` (`/data/ranking.db`) | SQLite database file path (initializes current schema on clean start) |
| `RANKING_MODELS_PATH` | `config/models.json` (`/app/config/models.json`) | Required canonical model and alias catalog |
| `RANKING_WEB_DIR` | unset (`/app/web`) | Built frontend asset directory; omit for API-only mode |
| `RANKING_CACHE_REFRESH_INTERVAL` | `5m` | Periodic leaderboard cache refresh interval |
| `RANKING_SHUTDOWN_TIMEOUT` | `10s` | Graceful drain deadline per shutdown stage |
| `RANKING_TRUSTED_PROXIES` | unset (none) | Comma-separated trusted reverse-proxy IPs or CIDRs |
| `RANKING_PUBLIC_ORIGIN` | unset | Public HTTPS origin; configure together with both GitHub variables |
| `RANKING_GITHUB_CLIENT_ID` | unset | GitHub OAuth App client ID |
| `RANKING_GITHUB_CLIENT_SECRET` | unset | GitHub OAuth App secret; configure at runtime only |
| `RANKING_REGISTRATION_ENABLED` | `true` | Permit starts and new writer bindings when OAuth is configured; existing writers are unaffected |
| `RANKING_REGISTRATION_IPV6_64` | `true` | Group native IPv6 sources by `/64` prefix for admission rate limits |
| `RANKING_SETTLED_DAY_RETENTION_DAYS` | `31` | Retention window (`1`–`36500` days) for finalized daily total summaries |
| `RANKING_ADMIN_CONFIG_FILE` | unset (disabled) | Path to private owner credentials JSON outside `RANKING_WEB_DIR` |

Registration start budgets and report concurrency/bucket settings are documented in **[Registration API Configuration](docs/registration-api.md#startup-configuration)**, **[Registration Admission](docs/registration-admission.md)**, and **[Daily Report Snapshots](docs/report-snapshots.md)**.

### Health & Readiness Probes

- `GET /healthz`: Returns `200 {"status":"ok"}` when the HTTP process is alive.
- `GET /readyz`: Returns `200 {"status":"ready"}` only after SQLite, catalogs, and the **first complete leaderboard cache round** succeed (used by the container `HEALTHCHECK`).

## Benchmark

In a historical test with a cgroup limit of 2 vCPU and 1 GiB RAM (zero swap), 20,000+ legacy identities and injected backlog, the tested build sustained **~500 logical daily-snapshot updates/sec** over a 20-minute continuous run while draining backlog. Hardware details, workload distributions, latency percentiles, and scope caveats are documented in the **[500 TPS Sustained Benchmark Report (中文)](docs/benchmark.md)** and **[Capacity Tool Guide](docs/capacity-tool.md)**.

## Documentation

- **Client Integration**: [Integration Guide (中文)](docs/integration.md) · [Integration Flows (中文)](docs/integration-flow.md) · [Client Credentials](docs/client-credentials.md) · [Registration API](docs/registration-api.md) · [Display Names](docs/display-names.md) · [Daily Report Snapshots](docs/report-snapshots.md)
- **Rankings & Storage**: [Leaderboards & Scoring](docs/leaderboards.md) · [Model Catalog](docs/model-catalog.md) · [Avatar Catalog](docs/avatar-catalog.md) · [SQLite Storage](docs/storage.md) · [Client Table](docs/client-storage.md)
- **Operations & Security**: [Docker Deployment](docs/docker.md) · [Docker 部署 (中文)](docs/docker.zh.md) · [Admin Console (中文)](docs/admin.md) · [Registration Admission](docs/registration-admission.md) · [Registration Sources](docs/registration-sources.md) · [Benchmark (中文)](docs/benchmark.md)

## Development

<details>
<summary>Project structure, local development, and verification commands</summary>

### Project Structure

```text
cmd/ranking/           Production server entrypoint, wiring, and graceful shutdown
cmd/ranking-capacity/  Offline fixture generator and bounded load/verification CLI
config/models.json     Canonical model names, aliases, and reference price tiers
internal/
  admin/               Owner config, SQLite admin accounts, sessions, and ban/hide moderation
  auth/                Strict Bearer parsing and SHA-256 credential authentication
  avatars/             Embedded 66-avatar WebP catalog and in-memory serving
  cache/               Atomic multi-period snapshot cache and coalesced refresh worker
  clients/             Client persistence and literal cursor-paginated search
  httpserver/          Gin routes, security headers, os.OpenRoot static serving, Same-Origin/CSRF
  leaderboard/         5-board scoring, Wilson lower bound, positive-median TTFT, Top 100
  models/              Startup catalog validation and canonical/alias resolution
  periods/             UTC day/week/month/year boundary helpers
  registration/        GitHub registration transactions and fixed-window start rate limits
  reports/             Strict JSON decoder, digest, serialized SQLite writer, settlement & pruning
  statistics/          Consistent read-only snapshot merging finalized periods and pending days
  storage/             SQLite connection pools and direct current-schema initialization
web/                   React 19 + TypeScript + Tailwind CSS frontend and test suites
deploy/                CI gate, GHCR tag policy, and Docker smoke verification scripts
docs/                  Public protocol, storage, administration, and deployment documentation
```

### Prerequisites

- **Go 1.26.10+** with `CGO_ENABLED=1` and a C compiler (GCC / Clang for `go-sqlite3`)
- **Node.js 24.15+** (24.x) and `npm`

### Local Full-Stack Development

Run the Go backend and the Vite dev server in separate terminals (Vite proxies `/healthz`, `/readyz`, `/v1/leaderboards`, `/v1/admin`, and `/avatars/*` to `http://127.0.0.1:8080`):

```sh
# Terminal 1: API server
go run ./cmd/ranking

# Terminal 2: Frontend dev server
npm --prefix web ci
npm --prefix web run dev
```

Or build and serve both from the single Go binary:

```sh
npm --prefix web run build
go build -o bin/ranking ./cmd/ranking
RANKING_WEB_DIR=web/dist ./bin/ranking
```

### Verification Commands

```sh
make verify       # gofmt, go vet, Go unit tests, web typecheck/lint/Vitest, and both builds
make test-race    # Go race-detector suite (-race)
make test-browser # Isolated loopback server + Playwright browser end-to-end checks
```

To reuse an existing local Chromium binary, pass `PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH=/path/to/chromium make test-browser`. Icon asset details are in [docs/brand-icons.md](docs/brand-icons.md).

</details>

## License

This project is open source under the [MIT License](./LICENSE). Third-party source notices and licenses are listed in [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md).
