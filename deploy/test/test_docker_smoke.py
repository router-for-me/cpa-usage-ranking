"""Safety checks for the Docker smoke runner; no Docker daemon is used."""
import json
import sqlite3
import subprocess
from types import SimpleNamespace
import unittest
from unittest.mock import patch

import docker_smoke


class SmokeSafetyTests(unittest.TestCase):
    def make_smoke(self):
        return docker_smoke.Smoke(
            SimpleNamespace(image="example:test", arch="amd64", qemu=None, timeout=5), "."
        )

    def response(self, value, code=0):
        return subprocess.CompletedProcess([], code, json.dumps(value).encode(), b"")

    def schema_fixture(self):
        db = sqlite3.connect(":memory:")
        self.addCleanup(db.close)
        db.executescript("""
            CREATE TABLE clients(id INTEGER PRIMARY KEY, github_id INTEGER, cpa_uuid TEXT, withdrawn INTEGER, ranking_hidden INTEGER);
            CREATE TABLE client_writers(id INTEGER);
            CREATE TABLE registration_requests(id TEXT);
            CREATE TABLE models(id INTEGER);
            CREATE TABLE report_window(id INTEGER);
            CREATE TABLE daily_reports(day TEXT);
            CREATE TABLE daily_writer_versions(day TEXT);
            CREATE TABLE daily_stats(day TEXT);
            CREATE TABLE period_stats(kind TEXT);
            CREATE TABLE admin_accounts(id INTEGER);
        """)
        return db

    def test_current_schema_requires_tables_without_a_ledger(self):
        db = self.schema_fixture()
        docker_smoke.require_current_schema(db)
        db.execute("DROP TABLE admin_accounts")
        with self.assertRaisesRegex(AssertionError, "unexpected database schema"):
            docker_smoke.require_current_schema(db)
        db.execute("CREATE TABLE admin_accounts(id INTEGER)")
        db.execute("CREATE TABLE schema_migrations(version INTEGER)")
        with self.assertRaisesRegex(AssertionError, "unexpected database schema"):
            docker_smoke.require_current_schema(db)

    def test_cleanup_refuses_unowned_container_and_volume(self):
        smoke = self.make_smoke()
        smoke.containers = ["collision"]
        smoke.volume_created = True

        def respond(*args, **kwargs):
            if args[:2] == ("container", "inspect"):
                return self.response([{"Config": {"Labels": {docker_smoke.LABEL: "another-run"}}}])
            if args[:2] == ("volume", "inspect"):
                return self.response([{"Labels": {}}])
            self.fail(f"unexpected destructive operation: {args}")

        with patch.object(docker_smoke, "docker", side_effect=respond):
            with self.assertRaisesRegex(RuntimeError, "refusing to remove container.*refusing to remove volume"):
                smoke.cleanup()

    def test_cleanup_removes_only_owned_resources(self):
        smoke = self.make_smoke()
        smoke.containers = ["owned"]
        smoke.volume_created = True
        labels = {docker_smoke.LABEL: smoke.run_id}

        def respond(*args, **kwargs):
            if args[:2] == ("container", "inspect"):
                return self.response([{"Config": {"Labels": labels}}])
            if args[:2] == ("volume", "inspect"):
                return self.response([{"Labels": labels}])
            return self.response([])

        with patch.object(docker_smoke, "docker", side_effect=respond) as call:
            smoke.cleanup()
        calls = [item.args for item in call.call_args_list]
        self.assertEqual([a for a in calls if a[0] == "rm"], [("rm", "-f", "owned")])
        self.assertEqual([a for a in calls if a[:2] == ("volume", "rm")],
                         [("volume", "rm", smoke.volume)])

    def test_cleanup_does_not_delete_uninspectable_volume(self):
        smoke = self.make_smoke()
        smoke.volume_created = True
        with patch.object(docker_smoke, "docker", return_value=self.response([], code=1)) as call:
            with self.assertRaisesRegex(RuntimeError, "cannot verify volume ownership"):
                smoke.cleanup()
        self.assertEqual([item.args for item in call.call_args_list], [("volume", "inspect", smoke.volume)])

    def test_runtime_has_bounded_cpu_and_memory(self):
        smoke = self.make_smoke()
        with patch.object(docker_smoke, "text") as call:
            smoke.run("owned")
        args = call.call_args.args
        for key, value in [("--cpus", "2"), ("--memory", "1g"), ("--memory-swap", "1g")]:
            self.assertEqual(args[args.index(key) + 1], value)


if __name__ == "__main__":
    unittest.main()
