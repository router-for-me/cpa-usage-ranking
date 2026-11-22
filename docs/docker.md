# Docker deployment

[简体中文](docker.zh.md) · [README](../README.md)

The image supports Linux `amd64` and `arm64`. It contains the CGO/SQLite Go
service, built frontend (including third-party notices), embedded avatars and
the model catalog; Node and compilers are not required at runtime. The two
architectures were built and functionally checked; ARM64 was checked with
explicit QEMU user-mode emulation, not on native ARM hardware. There is no
standalone binary release.

## Build and start

With Docker Engine, Buildx and Compose installed, from the repository root:

```sh
docker compose -p cpa-ranking up -d --build
docker compose -p cpa-ranking ps
docker compose -p cpa-ranking logs ranking
```

The example builds `cpa-usage-ranking:local` for the builder's platform,
persists `/data` in `cpa-ranking_ranking-data`, and publishes port 8080 only at
`127.0.0.1:8080`. To build a particular architecture without running it:

```sh
docker buildx build --platform linux/amd64 --load -t cpa-usage-ranking:local .
# Or replace linux/amd64 with linux/arm64.
```

The native build stages use a target C cross-compiler when needed; the final
image has no target-architecture build commands. Executing a foreign
architecture still requires a suitable emulator or native host. The build
fetches base images and build dependencies. A Buildx builder that can load a
single-platform image into the local engine is required for `--load`.

## GHCR images and publication

When an image has actually been published to GHCR, set `RANKING_IMAGE` to its
fully qualified reference. Replace the example owner, repository and version
with the real values. The repository path is lowercase. Private packages require
appropriate GHCR access; public availability is a package setting.

```sh
export RANKING_IMAGE=ghcr.io/owner/repository:v1.2.3
docker compose -p cpa-ranking pull ranking
docker compose -p cpa-ranking up -d --no-build
```

The default, without `RANKING_IMAGE`, remains a local build. Use `--no-build`
with a registry image so Compose does not replace it with a local build. Image
tags can be moved or republished; for reproducible deployment use a verified
manifest digest, for example `ghcr.io/owner/repository@sha256:<digest>`, and
protect Git release tags. Publishing workflows do not create a GitHub Release
or distribute standalone binaries.

The configured GHCR workflow has the following policy:

- A pushed stable Git tag must be exactly `vMAJOR.MINOR.PATCH`, with no leading
  zeroes except `0`, and its commit must be reachable from the repository's
  default branch. Annotated and lightweight tags are supported. Prereleases and
  tags on an unmerged branch are rejected. It publishes `vMAJOR.MINOR.PATCH` and
  `sha-<full-commit-sha>`.
- A manually dispatched publication must select a non-default branch. It
  publishes `dev-sha-<full-commit-sha>` and, if an optional development name
  is provided, `dev-<name>`. Names must be lowercase Docker-tag characters,
  start with a letter or digit, and be at most 124 characters; reserved names,
  version-like names and `sha-` names are rejected. Empty means SHA tag only.
- No `latest`, numeric version, major or minor aliases are published. This
  deliberately prevents an older version from rolling back a shared stable
  channel; consumers choose an exact version or digest. Development names cannot
  enter the stable namespace. Development name tags can move on subsequent runs.
- Each publication requires policy validation and the entire reusable CI to
  succeed; failed, cancelled or skipped required checks block it. CI runs Go,
  frontend and policy tests and builds/runs both image architectures. ARM64 is
  QEMU-emulated on a GitHub-hosted AMD64 runner, not native hardware. Browser
  tests are not part of this workflow. The final multi-platform publish is a
  separate rebuild, so its resulting digest is not the tested local image digest.
- Only the publishing job has `packages: write`; it uses `GITHUB_TOKEN` and
  derives the image name from the repository. PR and ordinary CI do not log in
  or push to GHCR. Buildx caches are stored in the GitHub Actions cache, not
  GHCR. Publications are serialized; GitHub may replace an already-pending run,
  so check the run status and rerun an omitted publication if needed.

The workflow is a publication configuration, not a claim that any image or
package is currently available. Branch and tag protection, package visibility
and repository Actions permissions remain administrator responsibilities.

**Do not expose this HTTP service directly to the public internet.** Registration
and reports carry Bearer credentials. Use an HTTPS-terminating reverse proxy,
restrict direct container and loopback access, and keep Authorization and bodies
out of proxy logs. The image listens on all container interfaces; other
containers with network access may reach it even when the host port is bound to
loopback. Forwarded client IPs are untrusted by default: configure only actual
trusted proxy IPs/CIDRs as explained in [registration sources](registration-sources.md).

The Compose example does not impose CPU or memory limits. Set `cpus`,
`mem_limit` and `memswap_limit` for your host budget; these limits are not a
throughput guarantee. The smoke checks use a 2-CPU quota and a 1 GiB memory
limit with no additional swap, not a minimum production sizing requirement.

## Data, permissions and stopping

The service runs as UID/GID `10001:10001`, not root. `/data` is initialized
with owner `10001:10001` and mode `0700` in a new Docker-managed volume; the
database may have a less restrictive file mode but is protected by that
directory. Existing volumes, bind mounts, rootless UID mappings and SELinux
policy must allow this UID to write; the image does not recursively change
ownership on startup. For a host bind mount, an administrator can prepare an
empty directory with the appropriate mapped ownership and mode, for example
`install -d -m 0700 -o 10001 -g 10001 /srv/cpa-ranking` on a conventional
rootful Linux host. Protect it from other users.

Use one service instance and local persistent storage, not an NFS/network
share or multiple replicas sharing the database. SQLite uses WAL: preserve its
`-wal` and `-shm` sidecars while open. Do not delete them or copy a live DB
file by itself. Stopping the container sends SIGTERM to the service. Compose
allows 30 seconds by default, while `RANKING_SHUTDOWN_TIMEOUT` defaults to
10 seconds for each applicable drain stage; increase the Docker stop grace
period with sufficient headroom if changing it. A forced kill or timeout is not
a clean shutdown. `docker compose down -v` deletes the data volume: avoid it
unless that deletion is intentional.

## Configuration and health

Pass service settings through Compose `environment` or `docker run -e`;
exporting them in the host shell does not automatically pass them to a container.
`RANKING_IMAGE` is a Compose image-selection variable, not a service setting.

These **image** defaults differ from the source-run defaults in the README:

| Variable | Image default | Purpose |
| --- | --- | --- |
| `RANKING_HTTP_ADDR` | `0.0.0.0:8080` | Container listener; the Compose host mapping is separately loopback-only |
| `RANKING_SQLITE_PATH` | `/data/ranking.db` | SQLite file on the persistent volume |
| `RANKING_WEB_DIR` | `/app/web` | Built frontend directory |
| `RANKING_MODELS_PATH` | `/app/config/models.json` | Required model catalog |
| `RANKING_HEALTHCHECK_URL` | `http://127.0.0.1:8080/readyz` | Image health-check target, not a service setting |

If overriding the listener/port, adjust both the Compose port mapping and
`RANKING_HEALTHCHECK_URL` to a reachable local readiness URL. The image health
check uses `/readyz`: database and catalogs must initialize and the first real
cache round must complete. `/healthz` only indicates liveness. A later failed
cache refresh retains the previous successful round, so readiness does not
promise freshness. Other settings, including shutdown and production-default
registration/report limits, are listed in the [README](../README.md#configuration),
[registration API](registration-api.md) and [report API](report-snapshots.md).
Explicit empty/invalid settings and missing required files fail startup.

## GitHub OAuth App

1. Create a GitHub OAuth App. Its authorization callback URL must be the
   public Ranking origin followed by `/auth/github/callback`, for example
   `https://ranking.example.com/auth/github/callback`. This is **not** the
   user's `return_to` URL. Do not configure repository or email scopes.
2. Pass `RANKING_PUBLIC_ORIGIN=https://ranking.example.com`,
   `RANKING_GITHUB_CLIENT_ID`, and `RANKING_GITHUB_CLIENT_SECRET` together when
   starting the container. `compose.yaml` passes these variables (and the
   optional `RANKING_REGISTRATION_ENABLED`) through from the deployment
   environment. For `docker run`, use the corresponding `-e` options or a
   protected runtime env file. Do not commit secrets, put them in images,
   or print resolved service settings that contain them.
3. Route public HTTPS requests for `/auth/github/*` and the API to the same
   Ranking instance. Exclude Authorization, cookies, bodies and full OAuth
   query strings from proxy and application observability (log the path only
   on `/auth/github/*`). Allow the service to reach `github.com` and
   `api.github.com` over HTTPS. The Ranking process does not contact client
   `return_to` URLs.

With the OAuth variables unset or `RANKING_REGISTRATION_ENABLED=false`, new
registrations are closed; public reads, existing writer reports and withdrawal
continue. Partial OAuth configuration fails startup. Use a **fresh data volume**
when replacing a build with the former registration schema; there is no upgrade
or writer backfill. Preserve the original volume separately as needed.

## Offline backup and restore

Before upgrading, stop the service cleanly and verify that it exited
successfully. The following example archives the **entire** default volume while
it is stopped; run from the repository root. Store and protect the archive in a
separate durable location. Also preserve deployment settings and any custom
model catalog separately. Clients must preserve their own credentials: the
server stores only their hashes.

```sh
docker compose -p cpa-ranking stop -t 30 ranking
# Confirm this prints 0 before taking the backup.
docker inspect --format '{{.State.ExitCode}}' "$(docker compose -p cpa-ranking ps -aq ranking)"
docker volume inspect cpa-ranking_ranking-data >/dev/null
umask 077
BACKUP_DIR="$HOME/cpa-ranking-backups"
mkdir -p "$BACKUP_DIR" && chmod 700 "$BACKUP_DIR"
BACKUP_FILE="$BACKUP_DIR/ranking-data-$(date -u +%Y%m%dT%H%M%SZ).tar"
docker run --rm --network none \
  --mount type=volume,src=cpa-ranking_ranking-data,dst=/data,readonly \
  --entrypoint /bin/busybox "${RANKING_IMAGE:-cpa-usage-ranking:local}" \
  tar -C /data -cf - . > "$BACKUP_FILE"
# Resume the existing service when appropriate:
docker compose -p cpa-ranking start ranking
```

To restore, stop the service again and extract a **trusted** backup into a new,
empty volume; do not overwrite the original or a running database. For example:

```sh
docker compose -p cpa-ranking stop -t 30 ranking
BACKUP_FILE=/absolute/path/to/verified-backup.tar
# Choose another name if this volume already exists.
docker volume inspect cpa-ranking-restored >/dev/null 2>&1 && { echo 'volume already exists'; exit 1; }
docker volume create cpa-ranking-restored
docker run --rm -i --network none --user 0:0 \
  --mount type=volume,src=cpa-ranking-restored,dst=/data \
  --entrypoint /bin/busybox "${RANKING_IMAGE:-cpa-usage-ranking:local}" \
  sh -c 'tar -C /data -xf - && chown -R 10001:10001 /data && chmod 700 /data' \
  < "$BACKUP_FILE"
```

Point Compose at that volume using an additional `compose.restore.yaml`:

```yaml
volumes:
  ranking-data:
    external: true
    name: cpa-ranking-restored
```

```sh
docker compose -p cpa-ranking -f compose.yaml -f compose.restore.yaml up -d --no-build
```

Check readiness and expected data. The old volume and backup remain available;
restoring loses changes made after the backup. Startup supports a new database or
one with the current schema; it does not upgrade or downgrade older schemas.
Do not assume an incompatible database can be used with another image.

## Repeatable image check

On a Linux Docker host with Python 3, this creates uniquely labelled disposable
containers and a volume, publishes no ports, and removes its test resources:

```sh
mkdir -p /path/to/test-work
python3 deploy/test/docker_smoke.py --image cpa-usage-ranking:local \
  --arch amd64 --work-dir /path/to/test-work
```

Use `--arch arm64` for an ARM64 image. On an AMD64 host without ARM binfmt,
provide `--qemu /absolute/path/to/qemu-aarch64-static`; the script explicitly
invokes it and disables Docker's in-container health check for that emulated run
while checking `/readyz` over HTTP. It does not install or register an emulator.
The checks cover schema initialization, UI and avatar assets, real
registration (with offline approval, no GitHub traffic), reporting and withdrawal,
recreation and persistence, permissions, clean SIGTERM,
and startup failures; they are not a load or capacity test.
