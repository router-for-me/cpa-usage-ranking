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
  Open-source AI usage leaderboard server for the <a href="https://github.com/router-for-me/CLIProxyAPI">CLIProxyAPI (CPA)</a> ecosystem.<br />
  Built for third-party collectors, gateways, and dashboards to report aggregated daily usage and display public rankings over HTTP JSON.
</p>

<p align="center">
  <strong><a href="#client-integration">Client Integration</a> · <a href="#leaderboards">Leaderboards</a> · <a href="#documentation">Documentation</a></strong>
</p>

## Client Integration

Clients integrate over standard **HTTPS + UTF-8 JSON (optional gzip on registration and reports)**, with no SDK requirement or coupling to any specific client codebase:

- **Read-only dashboards**: Call `GET /v1/leaderboards/{board}/{period}` and `/avatars/*` directly—no registration or credentials required.
- **Usage reporters**: Persist one 32-byte `base64url` Bearer secret per installation, bind `(GitHub user ID, CPA server ID)` through GitHub OAuth, and upload complete UTC daily cumulative snapshots.

| Step | Endpoint | Auth | Purpose |
| --- | --- | --- | --- |
| Start registration | `POST /v1/clients/register/start` | Writer `Bearer` | Submit `server_id`, `return_to`, and optional `avatar_id`; receive `authorize_url` |
| Complete registration | `POST /v1/clients/register/complete` | Same `Bearer` | Bind seat (`201` new, `200` reused/recovered, `202` pending) |
| Push daily snapshot | `PUT /v1/clients/report/{utc_day}` | Registered `Bearer` | Atomically replace the seat's full UTC today/yesterday snapshot |
| Withdraw seat | `POST /v1/clients/withdraw` | Registered `Bearer` | Withdraw the entire seat (`204`, no response body); repeat registration with the same GitHub account and `server_id` to rejoin if not banned |
| Read leaderboards | `GET /v1/leaderboards/{board}/{period}` | None | Fetch the published Top-100 cached standings |
| Fetch avatars | `GET /avatars/catalog.json`, `/avatars/{path}` | None | Read the avatar catalog and static WebP images (`ETag` supported) |

**Key protocol guarantees:**
- **Client-owned credentials**: Only the `SHA-256` digest of each 32-byte secret is stored; multiple writers for one `(GitHub ID, server_id)` seat hold independent credentials.
- **Idempotent full snapshots**: Each writer maintains its own per-day `revision`; retries with matching content confirm safely without double-counting or overwriting newer data.
- **Privacy-minimal**: Only aggregated per-model token counts, execution/success counts, and mean latencies are accepted—never upstream API keys, prompts, or response payloads.

See the **[Client Integration Guide](docs/integration.md)** for sequence diagrams, full JSON validation, copyable cURL flows, error codes, and retry rules, and **[Model, Avatar, and Brand Catalogs](docs/catalogs.md)** for model matching and avatar assets.

## Leaderboards

- **Five boards**: `overall` (100-point composite score + 6 detail metrics), `usage` (exact int64 total tokens), `cache` (`cache_read / input`), `success` (95% Wilson lower bound), and `ttft` (mean first-token latency, lower is better).
- **Six UTC periods**: The web UI displays `today`, `yesterday`, `week` (previous complete Monday-start UTC week), and `month` (previous complete UTC calendar month); the API also serves `year` and `all`.
- **Precision & caching**: All IDs and metrics are serialized as exact decimal strings to preserve 64-bit integer precision. Public Top-100 snapshots are built atomically on a periodic schedule (default `5m`).

Full scoring formulas, tie-breaking, and UI formatting rules are documented in **[Leaderboards and Scoring](docs/leaderboards.md)**.

## Documentation

| Document | Scope |
| --- | --- |
| [Client Integration Guide](docs/integration.md) | Endpoints, OAuth & multi-writer flows, snapshot schema, cURL, errors & retries |
| [Leaderboards and Scoring](docs/leaderboards.md) | 5 boards, 6 UTC periods, scoring formulas, tie-breaking, UI formatting |
| [Model, Avatar, and Brand Catalogs](docs/catalogs.md) | Canonical model & alias matching, embedded WebP avatars, brand assets |
| [Deployment and Configuration Guide](docs/deployment.md) | Docker Compose, GHCR policy, GitHub OAuth, reverse proxy, full env vars, backups |
| [Administrator Console](docs/admin.md) | Optional moderation console: owner/DB admins, user search, ban & exclude rules |
| [Storage, Settlement, and Caching](docs/storage.md) | SQLite WAL pools, schema, serialized writer, UTC settlement & retention, cache |
| [Benchmark and Capacity Testing](docs/benchmark.md) | 500 TPS sustained ingestion benchmark reference & `cmd/ranking-capacity` CLI |

> **Deploying a ranking server instance?** Multi-arch Docker Compose (`linux/amd64`, `linux/arm64`), GHCR release tags, GitHub OAuth App setup, and full server configuration are covered in the [Deployment and Configuration Guide](docs/deployment.md).

## License

Licensed under the [MIT License](./LICENSE). See [THIRD_PARTY_NOTICES.md](./THIRD_PARTY_NOTICES.md) for third-party notices.
