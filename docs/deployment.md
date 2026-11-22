# Deployment and Configuration Guide

[简体中文](deployment.zh.md) · [README](../README.md)

The container image supports Linux `amd64` and `arm64`, bundling the static CGO/SQLite Go binary, built web frontend (including third-party notices), embedded avatars, and model catalog. No host Go, Node.js, or C compiler is required at runtime. No standalone binary tarballs are distributed.

> CI builds and smoke-tests both architectures; ARM64 smoke verification runs under QEMU user-mode emulation on an AMD64 runner rather than native ARM hardware.

## Docker Compose Quick Start

### Build and Run Locally

From the repository root (requires Docker Engine, Buildx, and Compose):

```sh
docker compose -p cpa-ranking up -d --build
docker compose -p cpa-ranking ps
docker compose -p cpa-ranking logs ranking
```

The default `compose.yaml` builds `cpa-usage-ranking:local`, persists `/data` in the named volume `cpa-ranking_ranking-data`, and binds port `8080` to `127.0.0.1:8080` only. To build a specific platform without running it:

```sh
docker buildx build --platform linux/amd64 --load -t cpa-usage-ranking:local .
# Replace linux/amd64 with linux/arm64 for ARM64.
```

### Run a Published GHCR Image

Set `RANKING_IMAGE` to an exact release tag or immutable manifest digest (replace `<owner>/<repo>` and version with real values; repository paths are lowercase):

```sh
export RANKING_IMAGE=ghcr.io/<owner>/<repo>:v1.0.0
docker compose -p cpa-ranking pull ranking
docker compose -p cpa-ranking up -d --no-build
```

Use `--no-build` so Compose does not rebuild locally. Registry tags can still be republished; for strictly immutable deployments, pin a verified manifest digest (`ghcr.io/<owner>/<repo>@sha256:<digest>`).

### GHCR Publication Policy

- **Stable tags**: Pushing a strict `vMAJOR.MINOR.PATCH` tag (no leading zeroes except `0`, no prerelease suffix) reachable from the default branch publishes `:vMAJOR.MINOR.PATCH` and `:sha-<40-char-commit>`.
- **Development tags**: Manual `workflow_dispatch` on a non-default branch publishes `:dev-sha-<40-char-commit>` and, when a valid lowercase `dev_tag` is given, `:dev-<name>`.
- **No floating aliases**: `:latest`, `:v1`, and `:v1.x` are never published so consumers always pick an exact version or digest.
- **CI gate**: Every publish job runs policy checks and the full reusable CI (Go, frontend, policy tests, and `amd64`/`arm64` smoke tests) before logging into GHCR with `GITHUB_TOKEN`.

## GitHub OAuth App

1. Create a **GitHub OAuth App** with Authorization callback URL `https://<public-ranking-origin>/auth/github/callback` (for example `https://ranking.example.com/auth/github/callback`). This is **not** a client's `return_to` URL. Request no repository or email scopes.
2. Supply `RANKING_PUBLIC_ORIGIN`, `RANKING_GITHUB_CLIENT_ID`, and `RANKING_GITHUB_CLIENT_SECRET` together as private runtime environment variables (never commit them or bake them into images). `compose.yaml` forwards these three variables and optional `RANKING_REGISTRATION_ENABLED` from the deployment environment.
3. Route `/auth/github/*` and `/v1/*` on the same HTTPS origin to the same Ranking instance. Ensure Ranking can reach `github.com` and `api.github.com` over HTTPS; it never calls client `return_to` URLs.

If all three OAuth variables are unset—or `RANKING_REGISTRATION_ENABLED=false`—new registrations stay closed while public reads, existing writer reports, and withdrawal continue. Partial or invalid OAuth configuration fails startup.

## Reverse Proxy, Security, and Source Resolution

- **Terminate TLS in front of Ranking**: Registration, reporting, and admin login transmit credentials. Never expose the container's HTTP listener directly to the public internet. Even when the host port binds `127.0.0.1:8080`, the container listens on `0.0.0.0:8080` inside the Docker network—restrict container-network access appropriately.
- **Scrub sensitive proxy logs**: Exclude `Authorization` headers, cookies, request bodies, and full `/auth/github/*` query strings (log path only on OAuth routes).
- **Trusted proxies (`RANKING_TRUSTED_PROXIES`)**: Unset by default (all forwarded headers are ignored and only the connection's `RemoteAddr` is used). Behind a reverse proxy, explicitly list only your actual proxy IPs or CIDRs (for example `127.0.0.1,10.2.0.0/24`). Only connections from trusted peers honor `X-Forwarded-For` (right-to-left to the first untrusted hop) or `X-Real-IP`; malformed headers fall back to `RemoteAddr`. Never trust `0.0.0.0/0` or client-accessible subnets.
- **IPv6 source grouping (`RANKING_REGISTRATION_IPV6_64`)**: Enabled (`true`) by default, masking native IPv6 addresses to `/64` (IPv4-mapped IPv6 is unmapped to IPv4) for registration start and admin login rate limits. Source keys are abuse-control keys only—never identity or account ownership.
- **Two-layer registration admission**: `POST /v1/clients/register/start` runs an in-memory per-source and global fixed-window gate **before** Bearer/body validation (resets on restart); requests rejected by per-source limits or a full in-memory source table (`429 registration_capacity`) do not spend global slots. Valid admitted requests then allocate a fixed `10m` SQLite transaction capped at `2,000` unexpired rows (not env-configurable); hitting that SQLite cap also returns `429 registration_capacity` after rate slots have been consumed, without evicting valid entries.

## Configuration Reference

Pass environment variables via Compose `environment` or `docker run -e` (host shell exports are not automatically forwarded unless referenced in `compose.yaml`). Explicitly empty or malformed values fail startup.

### Core Service and Health

| Variable | Source / Image Default | Purpose |
| --- | --- | --- |
| `RANKING_HTTP_ADDR` | Source: `127.0.0.1:8080`<br>Image: `0.0.0.0:8080` | HTTP listen address (`host:port`) |
| `RANKING_SQLITE_PATH` | Source: `data/ranking.db`<br>Image: `/data/ranking.db` | SQLite database file path on local storage |
| `RANKING_MODELS_PATH` | Source: `config/models.json`<br>Image: `/app/config/models.json` | Required canonical model and alias catalog |
| `RANKING_WEB_DIR` | Source: unset<br>Image: `/app/web` | Built frontend directory; omit when running from source for API-only mode |
| `RANKING_CACHE_REFRESH_INTERVAL` | `5m` | Periodic leaderboard cache rebuild interval |
| `RANKING_SHUTDOWN_TIMEOUT` | `10s` | Drain deadline per graceful shutdown stage |
| `RANKING_SETTLED_DAY_RETENTION_DAYS` | `31` | Retention days (`1`–`36500`) for finalized daily total summaries |
| `RANKING_HEALTHCHECK_URL` | Image: `http://127.0.0.1:8080/readyz` | Container `HEALTHCHECK` URL only (not read by the Go binary) |

- **Probes**: `GET /healthz` returns `200 {"status":"ok"}` while alive. `GET /readyz` returns `200 {"status":"ready"}` once SQLite, catalogs, and the initial cache round succeed, or `503 {"status":"not_ready","reason":"initializing_or_unavailable"}` before readiness or if SQLite becomes unavailable (used by image `HEALTHCHECK`; update `RANKING_HEALTHCHECK_URL` if changing ports).

### Reverse Proxy and GitHub OAuth Registration

| Variable | Default | Purpose |
| --- | --- | --- |
| `RANKING_TRUSTED_PROXIES` | unset (trust none) | Comma-separated trusted reverse-proxy IPs or CIDRs |
| `RANKING_REGISTRATION_IPV6_64` | `true` | Group native IPv6 rate-limit keys by `/64` prefix |
| `RANKING_PUBLIC_ORIGIN` | unset | Public HTTPS origin (`https://host[:port]`, no path/query/fragment) |
| `RANKING_GITHUB_CLIENT_ID` | unset | GitHub OAuth App client ID |
| `RANKING_GITHUB_CLIENT_SECRET` | unset | GitHub OAuth App client secret (runtime only) |
| `RANKING_REGISTRATION_ENABLED` | `true` | Permit starts and new writer bindings when OAuth is configured |
| `RANKING_REGISTRATION_MAX_BODY_BYTES` | `4096` | Max JSON body bytes for registration `start` and `complete` |
| `RANKING_REGISTRATION_SOURCE_REQUESTS` | `20` | Max `/register/start` attempts per source per window |
| `RANKING_REGISTRATION_GLOBAL_REQUESTS` | `200` | Max `/register/start` attempts globally per window |
| `RANKING_REGISTRATION_REQUEST_WINDOW` | `1m` | Fixed window duration for registration start limits |
| `RANKING_REGISTRATION_REQUEST_MAX_SOURCES` | `10000` | Max tracked in-memory registration source entries |

### Daily Report Ingestion

| Variable | Default | Purpose |
| --- | --- | --- |
| `RANKING_REPORT_MAX_BODY_BYTES` | `131072` | Max uncompressed JSON body bytes per report |
| `RANKING_REPORT_MAX_ITEMS` | `128` | Max raw model items per report (before catalog matching) |
| `RANKING_REPORT_DECODE_CONCURRENCY` | `16` | Max concurrent request body reads and decodes |
| `RANKING_REPORT_QUEUE_CAPACITY` | `128` | Pending serialized report write queue length |
| `RANKING_REPORT_TRANSACTION_TIMEOUT` | `5s` | Execution timeout per report or maintenance SQLite transaction |
| `RANKING_REPORT_CLIENT_INTERVAL` | `1m` | Per-seat token refill interval (shared by all writers of a seat) |
| `RANKING_REPORT_CLIENT_BURST` | `2` | Per-seat token bucket burst capacity |
| `RANKING_REPORT_GLOBAL_INTERVAL` | `5ms` | Global report token refill interval |
| `RANKING_REPORT_GLOBAL_BURST` | `400` | Global report token bucket burst capacity |
| `RANKING_REPORT_MAX_CLIENTS` | `100000` | Max tracked in-memory per-seat rate states |
| `RANKING_REPORT_STATE_TTL` | `10m` | Idle expiry for refilled per-seat rate states |

### Admin Console

| Variable | Default | Purpose |
| --- | --- | --- |
| `RANKING_ADMIN_CONFIG_FILE` | unset (disabled) | Private owner JSON file outside `RANKING_WEB_DIR` (`0600`/`0640` or stricter read-only); see [Administrator Console](admin.md) |
| `RANKING_ADMIN_ALLOW_INSECURE_LOCAL_HTTP` | `false` | Local loopback test override only; requires literal loopback listen address |

## Data Permissions, Stopping, and Backups

- **Non-root identity (`10001:10001`)**: New Docker volumes initialize `/data` with `10001:10001` and mode `0700`. For host bind mounts, prepare ownership beforehand (for example `install -d -m 0700 -o 10001 -g 10001 /srv/cpa-ranking`); the container does not recursively `chown` on startup.
- **Single instance on local disk**: Never share one SQLite database across replicas or NFS/network filesystems. Preserve `-wal` and `-shm` sidecars while the database is open.
- **Graceful stop**: Compose allows `30s` (`stop_grace_period`) for `SIGTERM` while `RANKING_SHUTDOWN_TIMEOUT` defaults to `10s` per stage; raise `stop_grace_period` with headroom if increasing the timeout. Avoid `docker compose down -v` unless you intend to delete persistent data.
- **Resource limits**: Set `cpus`, `mem_limit`, and `memswap_limit` in Compose according to your host budget.

### Offline Backup

Stop the container cleanly, verify it reached `exited` with exit code `0`, resolve the actual `/data` volume mounted on the container (so backups still target the active volume after any restore), write to a temporary archive, and rename atomically on success:

```sh
docker compose -p cpa-ranking stop -t 30 ranking || exit 1
CID=$(docker compose -p cpa-ranking ps -aq ranking)
STATE=$(docker inspect --format '{{.State.Status}}:{{.State.ExitCode}}' "$CID")
[ "$STATE" = "exited:0" ] || { echo "unexpected container state: $STATE"; exit 1; }
DATA_VOLUME=$(docker inspect \
  --format '{{range .Mounts}}{{if eq .Destination "/data"}}{{.Name}}{{end}}{{end}}' \
  "$CID")
[ -n "$DATA_VOLUME" ] && docker volume inspect "$DATA_VOLUME" >/dev/null \
  || { echo 'missing /data volume'; exit 1; }

umask 077
BACKUP_DIR="$HOME/cpa-ranking-backups"
mkdir -p "$BACKUP_DIR" && chmod 700 "$BACKUP_DIR"
BACKUP_FILE="$BACKUP_DIR/ranking-data-$(date -u +%Y%m%dT%H%M%SZ).tar"
TMP_FILE="$BACKUP_FILE.tmp"
status=0
if docker run --rm --network none \
  --mount "type=volume,src=$DATA_VOLUME,dst=/data,readonly" \
  --entrypoint /bin/busybox "${RANKING_IMAGE:-cpa-usage-ranking:local}" \
  tar -C /data -cf - . > "$TMP_FILE"; then
  mv "$TMP_FILE" "$BACKUP_FILE"
else
  status=$?
  rm -f "$TMP_FILE"
fi
docker compose -p cpa-ranking start ranking || status=$?
exit "$status"
```

### Offline Restore

Extract a verified archive into a **new, empty volume** rather than overwriting the original:

```sh
docker compose -p cpa-ranking stop -t 30 ranking
BACKUP_FILE=/absolute/path/to/verified-backup.tar
docker volume inspect cpa-ranking-restored >/dev/null 2>&1 \
  && { echo 'volume already exists'; exit 1; }
docker volume create cpa-ranking-restored
docker run --rm -i --network none --user 0:0 \
  --mount type=volume,src=cpa-ranking-restored,dst=/data \
  --entrypoint /bin/busybox "${RANKING_IMAGE:-cpa-usage-ranking:local}" \
  sh -c 'tar -C /data -xf - && chown -R 10001:10001 /data && chmod 700 /data' \
  < "$BACKUP_FILE"
```

Point Compose at the restored volume via an override file `compose.restore.yaml`:

```yaml
volumes:
  ranking-data:
    external: true
    name: cpa-ranking-restored
```

```sh
docker compose -p cpa-ranking -f compose.yaml -f compose.restore.yaml up -d --no-build
```

## Repeatable Container Smoke Check

On a Linux Docker host with Python 3, verify an image end-to-end with isolated disposable containers and volumes (covers schema init, UI/avatars, offline-approved registration, reporting, withdrawal, persistence across recreation, permissions, `SIGTERM`, and fail-fast startup checks):

```sh
mkdir -p /path/to/test-work
python3 deploy/test/docker_smoke.py --image cpa-usage-ranking:local \
  --arch amd64 --work-dir /path/to/test-work
```

For ARM64 images, pass `--arch arm64` (and `--qemu /path/to/qemu-aarch64-static` on AMD64 hosts without registered ARM binfmt).
