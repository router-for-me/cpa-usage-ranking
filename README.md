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

<p align="center">
  Open-source, self-hosted AI usage leaderboard server and web dashboard for the <a href="https://github.com/router-for-me/CLIProxyAPI">CLIProxyAPI (CPA)</a> ecosystem.<br />
  Any compatible collector, gateway, or dashboard can integrate over HTTP JSON without client coupling or uploading API keys, prompts, or responses.
</p>

<p align="center">
  <strong><a href="#quick-start">Quick Start</a> · <a href="#client-integration">Client Integration</a> · <a href="#leaderboards--admin">Leaderboards &amp; Admin</a> · <a href="#configuration">Configuration</a> · <a href="#documentation">Documentation</a></strong>
</p>

## Features

- **Open Client Protocol**: Clients generate and persist their own 32-byte `base64url` Bearer secret (only its `SHA-256` digest is stored). GitHub OAuth binds `(GitHub user ID, CPA UUID)` into one seat supporting multiple independent writers.
- **Privacy-Minimal & Idempotent**: Accepts only aggregated per-model token counts, request/success counts, and mean latencies over UTC today/yesterday; per-writer `revision` tracking makes retries safe.
- **Five Boards & Complete UTC Periods**: Built-in **Overall, Usage, Cache Reuse, Reliability (95% Wilson), and TTFT** leaderboards across **Today, Yesterday, Weekly (previous complete UTC week), and Monthly (previous complete UTC month)** views with exact 64-bit integer precision.
- **Single-Binary SQLite Engine & Built-in UI**: Go + SQLite (`WAL`, `FULL` sync, serialized writer, background settlement, atomic cache) serving a responsive web dashboard, 66 embedded WebP avatars, and an opt-in administrator console.

## Quick Start

Docker Compose (`linux/amd64` and `linux/arm64`) bundles the static binary, web UI, avatars, and model catalog—no host Go or Node.js required.

```sh
# Build and start locally
docker compose -p cpa-ranking up -d --build
docker compose -p cpa-ranking ps
```

Or run an exact GHCR release tag (`vMAJOR.MINOR.PATCH` or `sha-<commit>`, with optional `dev-*` tags on manual non-default builds; no floating `latest` tag—pin `@sha256:<digest>` for strict immutability):

```sh
export RANKING_IMAGE=ghcr.io/<owner>/cpa-usage-ranking:v0.1.0
docker compose -p cpa-ranking pull ranking
docker compose -p cpa-ranking up -d --no-build
```

- **Production essentials**: Place an HTTPS reverse proxy in front of `127.0.0.1:8080`, set `RANKING_TRUSTED_PROXIES` to your proxy IP/CIDR, configure a GitHub OAuth App (`/auth/github/callback`) to enable new registrations, and run a single instance on local disk (never NFS). See [Deployment and Configuration Guide](docs/deployment.md).

## Client Integration

Clients integrate over standard HTTPS + uncompressed UTF-8 JSON. See the **[Client Integration Guide](docs/integration.md)** for sequence diagrams, complete field validation, error codes, and copyable cURL examples.

| Step | Endpoint | Auth | Purpose |
| --- | --- | --- | --- |
| Start registration | `POST /v1/clients/register/start` | Writer `Bearer <secret>` | Submit CPA UUID and `return_to`; open returned `authorize_url` |
| Complete registration | `POST /v1/clients/register/complete` | Same `Bearer <secret>` | Finalize seat creation (`201`), reuse/recovery (`200`), or poll (`202`) |
| Push daily snapshot | `PUT /v1/clients/report/{utc_day}` | Registered `Bearer` | Atomically replace the seat's full UTC today/yesterday snapshot |
| Withdraw seat | `POST /v1/clients/withdraw` | Registered `Bearer` | Permanently withdraw the whole seat (`204`, empty body) |
| Read leaderboards | `GET /v1/leaderboards/{board}/{period}` | None | Fetch the published Top-100 cached standings |

## Leaderboards & Admin

- **Boards**: `overall` (100-pt composite + 6 detail metrics), `usage` (exact int64 tokens), `cache` (`cache_read / input`), `success` (95% Wilson lower bound), and `ttft` (mean first-token latency, lower is better).
- **Periods**: Web UI displays `today`, `yesterday`, `week` (previous complete Monday-start UTC week), and `month` (previous complete UTC month); the API also serves `year` and `all`. See [Leaderboards and Scoring](docs/leaderboards.md).
- **Admin Console (opt-in)**: Disabled by default. Mount an owner JSON file (`0600`/`0640` or stricter read-only) outside `RANKING_WEB_DIR` and set `RANKING_ADMIN_CONFIG_FILE` to enable `/admin/leaderboards` (exclude/restore seats and reload published boards), `/admin/users` (search, ban/unban), and `/admin/accounts` (ordinary DB admins). See [Administrator Console](docs/admin.md).

## Configuration

Core variables are listed below (full rate-limit and ingestion tuning options are in [Deployment and Configuration Guide](docs/deployment.md#configuration-reference)):

| Variable | Source Default (Docker) | Description |
| --- | --- | --- |
| `RANKING_HTTP_ADDR` | `127.0.0.1:8080` (`0.0.0.0:8080`) | HTTP listen address |
| `RANKING_SQLITE_PATH` | `data/ranking.db` (`/data/ranking.db`) | SQLite database file path |
| `RANKING_MODELS_PATH` | `config/models.json` (`/app/config/models.json`) | Canonical model & alias catalog |
| `RANKING_WEB_DIR` | unset (`/app/web`) | Built frontend directory (omit when running from source for API-only) |
| `RANKING_TRUSTED_PROXIES` | unset | Trusted reverse-proxy IPs or CIDRs |
| `RANKING_PUBLIC_ORIGIN`<br>`RANKING_GITHUB_CLIENT_ID`<br>`RANKING_GITHUB_CLIENT_SECRET` | unset | GitHub OAuth settings (set all three to enable registration) |
| `RANKING_ADMIN_CONFIG_FILE` | unset | Private owner credentials JSON (enables Admin Console) |

Health probes: `GET /healthz` (process alive) and `GET /readyz` (`200` when SQLite, catalogs, and initial cache are ready; `503` otherwise).

## Documentation

| Document | Scope |
| --- | --- |
| [Client Integration Guide](docs/integration.md) | Endpoints, OAuth & reporting flows, snapshot schema, cURL, errors & retries |
| [Leaderboards and Scoring](docs/leaderboards.md) | 5 boards, 6 UTC periods, scoring formulas, tie-breaking, UI formatting |
| [Deployment and Configuration Guide](docs/deployment.md) | Docker Compose, GHCR policy, GitHub OAuth, proxy security, full env vars, backups |
| [Administrator Console](docs/admin.md) | Owner & DB admins, user search, ban/exclude matrix, CSRF/HTTPS security |
| [Storage, Settlement, and Caching](docs/storage.md) | SQLite WAL pools, schema, serialized writer, UTC settlement & retention, cache |
| [Model, Avatar, and Brand Catalogs](docs/catalogs.md) | `config/models.json` resolution, embedded WebP avatars, brand icon generation |
| [Benchmark and Capacity Testing](docs/benchmark.md) | 500 TPS sustained ingestion benchmark reference & `cmd/ranking-capacity` CLI |

## Development

<details>
<summary>Local prerequisites, dev commands, and verification</summary>

- **Prerequisites**: Go `1.26.10+` (`CGO_ENABLED=1` + GCC/Clang for `go-sqlite3`) and Node.js `24.15+` (`24.x`).
- **Full-stack dev** (Vite proxies `/healthz`, `/readyz`, `/v1/leaderboards`, `/v1/admin`, and `/avatars/*` to `http://127.0.0.1:8080`):
  ```sh
  go run ./cmd/ranking
  npm --prefix web ci && npm --prefix web run dev
  ```
- **Single-binary build**:
  ```sh
  npm --prefix web run build
  go build -o bin/ranking ./cmd/ranking
  RANKING_WEB_DIR=web/dist ./bin/ranking
  ```
- **Verification**:
  ```sh
  make verify       # gofmt, go vet, Go tests, web typecheck/lint/Vitest, builds
  make test-race    # Go race-detector suite (-race)
  make test-browser # Isolated loopback server + Playwright browser checks
  ```

</details>

## License

Licensed under the [MIT License](./LICENSE). See [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md) for third-party notices.
