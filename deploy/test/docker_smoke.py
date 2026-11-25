#!/usr/bin/env python3
"""Exercise an already-built image with a disposable Docker volume.

Linux Docker host, Python 3 and Docker CLI required. Pass a static user-mode
QEMU executable to run an ARM64 image on an AMD64 host without binfmt changes.
No ports are published, no production data is used, and resources are removed.
"""

import argparse
import base64
from contextlib import closing
import datetime as dt
import hashlib
import io
import json
import os
from pathlib import Path
import re
import secrets
import sqlite3
import subprocess
import sys
import tarfile
import tempfile
import time
import urllib.error
import urllib.request

LABEL = "io.cpa-usage-ranking.docker-smoke"
QEMU_IN_CONTAINER = "/usr/local/bin/qemu-aarch64-static"
class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, *_args):
        return None


HTTP = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
OAUTH_ORIGIN = "https://ranking.example"
OAUTH_ENV = ("RANKING_PUBLIC_ORIGIN=" + OAUTH_ORIGIN, "RANKING_GITHUB_CLIENT_ID=smoke-fixture",
             "RANKING_GITHUB_CLIENT_SECRET=smoke-fixture-not-a-secret")


def docker(*args, check=True, data=None, timeout=120):
    result = subprocess.run(
        ["docker", *args], input=data, stdout=subprocess.PIPE,
        stderr=subprocess.PIPE, timeout=timeout,
    )
    if check and result.returncode:
        raise RuntimeError(
            f"docker {args[0]} failed ({result.returncode}): "
            f"{result.stderr.decode(errors='replace')}"
        )
    return result


def text(*args, **kwargs):
    return docker(*args, **kwargs).stdout.decode().strip()


def require(condition, message):
    if not condition:
        raise AssertionError(message)


def require_current_schema(db):
    tables = {row[0] for row in db.execute("SELECT name FROM sqlite_schema WHERE type='table'")}
    expected = {"clients", "client_writers", "registration_requests", "models", "report_window",
                "daily_reports", "daily_writer_versions", "daily_stats", "period_stats", "admin_accounts"}
    require(expected <= tables and "schema_migrations" not in tables, f"unexpected database schema: {tables}")
    columns = {row[1] for row in db.execute("PRAGMA table_info(clients)")}
    require({"github_id", "server_id", "withdrawn", "ranking_hidden"} <= columns
            and "token_hash" not in columns and "cpa_uuid" not in columns,
            "unexpected seat columns")


def request(base, path, method="GET", body=None, secret=None):
    headers = {}
    if body is not None:
        body = json.dumps(body, separators=(",", ":")).encode()
        headers["Content-Type"] = "application/json"
    if secret is not None:
        headers["Authorization"] = "Bearer " + secret
    req = urllib.request.Request(base + path, data=body, headers=headers, method=method)
    try:
        with HTTP.open(req, timeout=5) as response:
            return response.status, response.read(), response.headers
    except urllib.error.HTTPError as error:
        return error.code, error.read(), error.headers


def expect_json(base, path, expected_status, method="GET", body=None, secret=None):
    status, content, _ = request(base, path, method, body, secret)
    require(status == expected_status, f"{method} {path}: HTTP {status}, expected {expected_status}")
    return json.loads(content)


class Smoke:
    def __init__(self, args, workdir):
        self.args = args
        self.workdir = Path(workdir)
        self.run_id = secrets.token_hex(6)
        self.prefix = "cpa-ranking-smoke-" + self.run_id
        self.volume = self.prefix + "-data"
        self.containers = []
        self.volume_created = False

    def image_args(self):
        args = ["--platform", "linux/" + self.args.arch]
        if self.args.qemu:
            args += ["--mount", f"type=bind,src={self.args.qemu},dst={QEMU_IN_CONTAINER},readonly",
                     "--entrypoint", QEMU_IN_CONTAINER, "--no-healthcheck"]
        return args

    def image_command(self):
        return [self.args.image, "/app/ranking"] if self.args.qemu else [self.args.image]

    def run(self, name, environment=()):
        self.containers.append(name)
        args = ["run", "-d", "--name", name, "--label", f"{LABEL}={self.run_id}",
                "--cpus", "2", "--memory", "1g", "--memory-swap", "1g",
                "--mount", f"type=volume,src={self.volume},dst=/data"]
        if not self.args.qemu:
            args += ["--health-interval", "2s", "--health-timeout", "5s",
                     "--health-start-period", "10s", "--health-retries", "3"]
        for variable in environment:
            args += ["--env", variable]
        text(*args, *self.image_args(), *self.image_command())

    def ready(self, name):
        deadline = time.monotonic() + self.args.timeout
        base = None
        last_error = ""
        while time.monotonic() < deadline:
            state = json.loads(text("inspect", "--format", "{{json .State}}", name))
            if not state["Running"]:
                logs = docker("logs", name, check=False).stderr.decode(errors="replace")
                raise RuntimeError(f"container exited before readiness: {state}; {logs}")
            ip = text("inspect", "--format", "{{range .NetworkSettings.Networks}}{{.IPAddress}}{{end}}", name)
            if ip:
                base = "http://" + ip + ":8080"
                try:
                    status, _, _ = request(base, "/readyz")
                    if status == 200 and (self.args.qemu or state.get("Health", {}).get("Status") == "healthy"):
                        return base
                    last_error = f"readyz={status}, health={state.get('Health', {}).get('Status')}"
                except (OSError, urllib.error.URLError) as error:
                    last_error = str(error)
            time.sleep(0.5)
        raise RuntimeError(f"container readiness timed out: {base} {last_error}")

    def exec_busybox(self, name, *args):
        prefix = [QEMU_IN_CONTAINER] if self.args.qemu else []
        return text("exec", name, *prefix, "/bin/busybox", *args)

    def stop(self, name):
        text("stop", "--time", "20", name, timeout=35)
        state = json.loads(text("inspect", "--format", "{{json .State}}", name))
        require(state["ExitCode"] == 0 and not state["OOMKilled"], f"unclean SIGTERM exit: {state}")
        logs = docker("logs", name).stderr.decode(errors="replace")
        require('"msg":"service stopped"' in logs, "missing graceful shutdown log")

    def invalid(self, label, environment, expected, extra_mount=None):
        name = self.prefix + "-invalid-" + label
        self.containers.append(name)
        args = ["run", "--name", name, "--network", "none", "--label", f"{LABEL}={self.run_id}",
                "--cpus", "2", "--memory", "1g", "--memory-swap", "1g",
                "--mount", f"type=volume,src={self.volume},dst=/data", "--env", environment]
        if extra_mount:
            args += ["--mount", extra_mount]
        result = docker(*args, *self.image_args(), *self.image_command(), check=False)
        message = result.stderr.decode(errors="replace")
        require(result.returncode != 0 and expected in message,
                f"invalid config {label} did not fail as expected: {result.returncode} {message}")

    def database_copy(self, name, dest):
        dest.mkdir()
        text("cp", f"{name}:/data/.", str(dest))
        require((dest / "ranking.db").is_file(), "database was not persisted")
        require(not (dest / "ranking.db-wal").exists() and not (dest / "ranking.db-shm").exists(),
                "unexpected SQLite sidecars after graceful shutdown")
        return dest / "ranking.db"

    def approve_offline(self, stopped, request_id):
        # Only the disposable, stopped volume is touched. The callback itself is
        # covered by fake RoundTripper in Go; never follow the GitHub redirect here.
        path = self.database_copy(stopped, self.workdir / "before-complete")
        with closing(sqlite3.connect(path)) as db:
            changed = db.execute("UPDATE registration_requests SET stage='approved',github_id=42,"
                                 "display_name='Docker Smoke',state=NULL,browser_hash=NULL,verifier=NULL "
                                 "WHERE id=? AND stage='authorizing'", (request_id,)).rowcount
            require(changed == 1, "failed to approve test transaction")
            db.commit()
            db.execute("PRAGMA wal_checkpoint(TRUNCATE)")
        archive = io.BytesIO()
        with tarfile.open(fileobj=archive, mode="w") as tar:
            item = tar.gettarinfo(str(path), arcname="ranking.db")
            item.uid, item.gid, item.mode = 10001, 10001, 0o600
            item.uname = item.gname = ""
            with path.open("rb") as file:
                tar.addfile(item, file)
        docker("cp", "-a", "-", f"{stopped}:/data", data=archive.getvalue())

    def cleanup(self):
        failures = []
        for name in reversed(self.containers):
            inspected = docker("container", "inspect", name, check=False)
            if inspected.returncode == 0:
                labels = json.loads(inspected.stdout)[0]["Config"].get("Labels") or {}
                if labels.get(LABEL) != self.run_id:
                    failures.append(f"refusing to remove container without this run's label: {name}")
                    continue
                result = docker("rm", "-f", name, check=False)
                if result.returncode:
                    failures.append(f"container {name}: {result.stderr.decode(errors='replace')}")
        if self.volume_created:
            inspected = docker("volume", "inspect", self.volume, check=False)
            if inspected.returncode:
                failures.append(f"cannot verify volume ownership: {self.volume}")
            elif (json.loads(inspected.stdout)[0].get("Labels") or {}).get(LABEL) != self.run_id:
                failures.append(f"refusing to remove volume without this run's label: {self.volume}")
            else:
                result = docker("volume", "rm", self.volume, check=False)
                if result.returncode:
                    failures.append(f"volume {self.volume}: {result.stderr.decode(errors='replace')}")
        if failures:
            raise RuntimeError("test resource cleanup failed: " + "; ".join(failures))

    def execute(self):
        inspect = json.loads(text("image", "inspect", self.args.image))[0]
        require(inspect["Os"] == "linux" and inspect["Architecture"] == self.args.arch,
                f"image platform mismatch: {inspect['Os']}/{inspect['Architecture']}")
        require(inspect["Config"]["User"] == "10001:10001", "image must run as UID/GID 10001")
        require(inspect["Config"]["Entrypoint"] == ["/app/ranking"], "service must be the image entrypoint")
        health = inspect["Config"]["Healthcheck"]
        require(health["Interval"] == 30_000_000_000 and health["Timeout"] == 3_000_000_000
                and health["StartPeriod"] == 15_000_000_000 and health["Retries"] == 3,
                "image healthcheck defaults")
        require("RANKING_HEALTHCHECK_URL=http://127.0.0.1:8080/healthz" in (inspect["Config"].get("Env") or []),
                "default container healthcheck URL must target /healthz")
        server_platform = text("version", "--format", "{{.Server.Os}}/{{.Server.Arch}}")
        if self.args.qemu:
            execution = "explicit QEMU user-mode emulation"
        elif server_platform == "linux/" + self.args.arch:
            execution = "native Docker host architecture"
        else:
            execution = "Docker runtime emulation (native not established)"
        print(f"image={inspect['Id']} platform=linux/{self.args.arch} "
              f"docker-host={server_platform} execution={execution}", flush=True)
        require(docker("volume", "inspect", self.volume, check=False).returncode != 0,
                "refusing to reuse an existing test volume")
        text("volume", "create", "--label", f"{LABEL}={self.run_id}", self.volume)
        require((json.loads(text("volume", "inspect", self.volume))[0].get("Labels") or {}).get(LABEL)
                == self.run_id, "test volume ownership mismatch")
        self.volume_created = True
        first, second = self.prefix + "-first", self.prefix + "-second"
        self.run(first, OAUTH_ENV)
        base = self.ready(first)
        require(expect_json(base, "/healthz", 200) == {"status": "ok"}, "liveness response")
        require(expect_json(base, "/readyz", 200) == {"status": "ready"}, "readiness response")
        status, html, _ = request(base, "/")
        require(status == 200 and b"<html" in html.lower(), "frontend index missing")
        assets = re.findall(rb'(?:src|href)="(/assets/[^"]+\.(?:js|css))"', html)
        require(assets and any(x.endswith(b".js") for x in assets), "built frontend assets missing")
        for asset in assets:
            require(request(base, asset.decode())[0] == 200, f"missing frontend asset {asset!r}")
        require(request(base, "/assets/third-party-notices.txt")[0] == 200, "frontend notices missing")
        catalog = expect_json(base, "/avatars/catalog.json", 200)
        require(catalog["default_avatar_id"] == 1 and len(catalog["avatars"]) == 66, "avatar catalog")
        status, avatar, headers = request(base, "/avatars/images/001.webp")
        require(status == 200 and avatar[:4] == b"RIFF" and avatar[8:12] == b"WEBP"
                and headers.get_content_type() == "image/webp", "avatar image")
        require(request(base, "/missing-page")[0] == 404, "unexpected frontend fallback")
        require(self.exec_busybox(first, "id", "-u") == "10001", "service UID")
        modes = self.exec_busybox(first, "stat", "-c", "%u:%g %a", "/data", "/data/ranking.db").splitlines()
        require(len(modes) == 2 and modes[0] == "10001:10001 700"
                and modes[1].startswith("10001:10001 "), f"volume/file ownership: {modes}")
        self.exec_busybox(first, "sh", "-c",
                          "test ! -e /usr/local/bin/node && test ! -e /usr/local/go/bin/go && "
                          "test ! -e /usr/bin/gcc && test ! -e /app/ranking-capacity && "
                          "test ! -e /plans && test ! -e /app/cmd && test ! -e /app/internal && "
                          "test ! -e /app/web/node_modules && test ! -e /app/web/test && "
                          "test -s /etc/ssl/certs/ca-certificates.crt")
        print("PASS: ready/liveness, image isolation, frontend/assets/notices, avatar, UID/data permissions "
              f"(/data={modes[0]}, database={modes[1]})", flush=True)

        raw_secret = secrets.token_bytes(32)
        secret = base64.urlsafe_b64encode(raw_secret).decode().rstrip("=")
        start, complete = "/v1/clients/register/start", "/v1/clients/register/complete"
        registration = {"server_id": "550e8400-e29b-41d4-a716-446655440000",
                        "return_to": "http://localhost:3001/settings?tab=ranking#status"}
        ticket = expect_json(base, start, 200, "POST", registration, secret)
        request_id = ticket["request_id"]
        auth_path = "/auth/github/authorize?request_id=" + request_id
        require(ticket["authorize_url"] == OAUTH_ORIGIN + auth_path, "configured authorize URL")
        status, _, headers = request(base, auth_path)
        require(status == 302 and headers.get("Referrer-Policy") == "no-referrer"
                and headers.get("Location", "").startswith("https://github.com/login/oauth/authorize?"),
                "fixed authorization redirect")
        completion = {"request_id": request_id}
        status, _, headers = request(base, complete, "POST", completion, secret)
        require(status == 202 and headers.get("Retry-After", "").isdigit(), "unapproved registration")
        self.stop(first)
        self.approve_offline(first, request_id)
        text("start", first)
        base = self.ready(first)
        registered = expect_json(base, complete, 201, "POST", completion, secret)
        client_id = registered["client_id"]
        require(client_id.isdecimal() and int(client_id) > 0 and registered["display_name"] == "Docker Smoke",
                "registration completion")
        require(request(base, "/v1/clients/register", "POST", {}, secret)[0] == 404, "old registration route")
        day = dt.datetime.now(dt.timezone.utc).date().isoformat()
        report_path = "/v1/clients/report/" + day
        require(request(base, "/v1/reports/" + day, "PUT", {}, secret)[0] == 404, "old report route")
        report = {"schema_version": 1, "revision": "7", "items": [{
            "model": "claude-fable-5", "total_tokens": "120",
            "input_tokens": "80", "output_tokens": "40", "cache_read_tokens": "60",
            "cache_write_tokens": "5", "reasoning_tokens": "10", "request_count": "2",
            "success_count": "2", "ttft_avg_ms": None, "ttft_samples": "0",
            "latency_avg_ms": 3000, "latency_samples": "2"}]}
        require(expect_json(base, report_path, 200, "PUT", report, secret) == {"revision": "7"}, "report")
        self.stop(first)
        first_db = self.database_copy(first, self.workdir / "before-restart")
        with closing(sqlite3.connect(first_db)) as db:
            require(db.execute("PRAGMA integrity_check").fetchone() == ("ok",), "SQLite integrity")
            require_current_schema(db)
            client = db.execute("SELECT token_hash,display_name FROM client_writers w JOIN clients c ON c.id=w.client_id "
                                "WHERE client_id=?", (client_id,)).fetchone()
            require(client == (hashlib.sha256(raw_secret).digest(), "Docker Smoke"), "persisted credential hash")
            revision = db.execute("SELECT revision,digest FROM daily_writer_versions WHERE day=? AND client_id=?",
                                  (day, client_id)).fetchone()
            stats = db.execute("SELECT model_id,total_tokens,input_tokens,output_tokens,request_count "
                               "FROM daily_stats WHERE day=? AND client_id=? ORDER BY model_id", (day, client_id)).fetchall()
            require(revision is not None and revision[0] == 7 and len(revision[1]) == 32, "persisted revision")
            require(len(stats) == 2 and stats[0] == (0, 120, 80, 40, 2) and stats[1][0] > 0
                    and stats[1][1:] == (120, 80, 40, 2), "persisted metrics")
        print("PASS: registration with offline approval, report, current schema and persisted credential/revision/metrics", flush=True)

        self.run(second)
        base = self.ready(second)
        require(expect_json(base, start, 503, "POST", registration, secret)["error"]["code"] == "registration_closed",
                "partial operation without OAuth")
        retried = expect_json(base, complete, 200, "POST", completion, secret)
        require(retried == registered, "credential retry changed identity after container recreation")
        require(expect_json(base, report_path, 200, "PUT", report, secret) == {"revision": "7"}, "report retry")
        older = dict(report, revision="6")
        conflict = expect_json(base, report_path, 409, "PUT", older, secret)
        require(conflict.get("revision") == "7", "revision conflict after restart")
        board = expect_json(base, "/v1/leaderboards/usage/all", 200)
        require(any(row["client_id"] == str(client_id) and row["metric"] == "120"
                    for row in board["entries"]), "recreated container lost usage metrics")
        for _ in range(2):
            require(request(base, "/v1/clients/withdraw", "POST", secret=secret)[0] == 204, "idempotent withdrawal")
        require(expect_json(base, report_path, 403, "PUT", report, secret)["error"]["code"] == "client_withdrawn",
                "withdrawal blocked original report retry")
        self.stop(second)
        second_db = self.database_copy(second, self.workdir / "after-restart")
        with closing(sqlite3.connect(second_db)) as db:
            require_current_schema(db)
            require(db.execute("SELECT count(*) FROM period_stats").fetchone() == (0,), "unexpected period data")
            require(db.execute("SELECT count(*) FROM admin_accounts").fetchone() == (0,), "unexpected admin data")
            require(db.execute("SELECT withdrawn,ranking_hidden FROM clients WHERE id=?", (client_id,)).fetchone()
                    == (1, 0), "withdrawal changed moderation flags or was not persisted")
            require(db.execute("SELECT revision,digest FROM daily_writer_versions WHERE day=? AND client_id=?",
                               (day, client_id)).fetchone()
                    == revision, "revision/digest changed after recreation")
            require(db.execute("SELECT model_id,total_tokens,input_tokens,output_tokens,request_count "
                               "FROM daily_stats WHERE day=? AND client_id=? ORDER BY model_id", (day, client_id)).fetchall()
                    == stats, "metrics changed after recreation")
        print("PASS: same-schema volume recreation, credential/revision/metrics and SIGTERM exit 0", flush=True)

        self.invalid("address", "RANKING_HTTP_ADDR=not-a-host-port", "RANKING_HTTP_ADDR")
        self.invalid("oauth-origin", "RANKING_PUBLIC_ORIGIN=http://ranking.example", "RANKING_PUBLIC_ORIGIN")
        self.invalid("models-missing", "RANKING_MODELS_PATH=/missing/models.json", "model")
        self.invalid("web-missing", "RANKING_WEB_DIR=/missing/web", "frontend")
        self.invalid("data-permissions", "RANKING_SQLITE_PATH=/not-writable/ranking.db", "create sqlite directory")
        bad_models = self.workdir / "bad-models.json"
        bad_models.write_text("not json", encoding="utf-8")
        bad_models.chmod(0o644)
        self.invalid("models-invalid", "RANKING_MODELS_PATH=/bad-models.json", "model",
                     f"type=bind,src={bad_models},dst=/bad-models.json,readonly")
        print("PASS: invalid settings, missing/invalid assets and unwritable data path fail startup", flush=True)


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--image", required=True)
    parser.add_argument("--arch", required=True, choices=("amd64", "arm64"))
    parser.add_argument("--work-dir", required=True, help="existing parent for temporary database copies")
    parser.add_argument("--qemu", help="absolute path to static qemu-aarch64 for explicit ARM64 emulation")
    parser.add_argument("--timeout", type=int, default=120)
    args = parser.parse_args()
    if args.qemu:
        args.qemu = str(Path(args.qemu).resolve(strict=True))
        require(args.arch == "arm64" and os.access(args.qemu, os.X_OK), "--qemu requires executable ARM64 emulator")
    require(Path(args.work_dir).is_dir(), "--work-dir must already exist")
    args.work_dir = str(Path(args.work_dir).resolve())
    with tempfile.TemporaryDirectory(prefix="docker-smoke-", dir=args.work_dir) as workdir:
        smoke = Smoke(args, workdir)
        try:
            smoke.execute()
        finally:
            smoke.cleanup()
    print("PASS: disposable test containers, volume and temporary database copies removed", flush=True)


if __name__ == "__main__":
    try:
        main()
    except (AssertionError, RuntimeError, OSError, subprocess.TimeoutExpired) as error:
        print(f"FAIL: {error}", file=sys.stderr)
        sys.exit(1)
