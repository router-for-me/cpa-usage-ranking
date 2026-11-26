import os
from pathlib import Path
import subprocess
import tempfile
import unittest


ROOT = Path(__file__).resolve().parents[2]
POLICY = ROOT / "deploy" / "ghcr_policy.py"
GATE = ROOT / "deploy" / "ci_gate.py"


class PublicationPolicyTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        cls.temp = tempfile.TemporaryDirectory(prefix="ghcr-policy-test-")
        cls.repo = Path(cls.temp.name) / "repository"
        cls.repo.mkdir()
        cls.base_env = dict(os.environ, GIT_CONFIG_NOSYSTEM="1", GIT_CONFIG_GLOBAL=os.devnull)
        cls.git("init", "-b", "main")
        cls.git("config", "user.name", "GHCR Policy Test")
        cls.git("config", "user.email", "test@example.invalid")
        cls.git("config", "commit.gpgsign", "false")
        cls.git("config", "tag.gpgsign", "false")
        cls.git("commit", "--allow-empty", "-m", "base")
        cls.old = cls.git("rev-parse", "HEAD")
        cls.git("tag", "v0.0.0")
        cls.git("commit", "--allow-empty", "-m", "default branch")
        cls.main = cls.git("rev-parse", "HEAD")
        cls.git("tag", "-a", "v2.3.4", "-m", "annotated")
        cls.git("update-ref", "refs/remotes/origin/main", cls.main)
        cls.git("checkout", "-b", "feature/test", cls.old)
        cls.git("commit", "--allow-empty", "-m", "unmerged feature")
        cls.feature = cls.git("rev-parse", "HEAD")
        cls.git("tag", "v2.3.5")

    @classmethod
    def tearDownClass(cls):
        cls.temp.cleanup()

    @classmethod
    def git(cls, *args):
        return subprocess.run(["git", *args], cwd=cls.repo, env=cls.base_env,
                              check=True, capture_output=True, text=True).stdout.strip()

    def setUp(self):
        self.output = Path(self.temp.name) / "output"
        self.output.write_text("", encoding="utf-8")
        self.env = dict(self.base_env, GITHUB_REPOSITORY="Example-Org/Ranking.Test",
                        GITHUB_SHA=self.main, GITHUB_REF="refs/tags/v2.3.4",
                        GITHUB_REF_NAME="v2.3.4", GITHUB_EVENT_NAME="push",
                        DEFAULT_BRANCH="main", DEV_TAG="", GITHUB_OUTPUT=str(self.output))
        self.git("checkout", "--detach", self.main)

    def run_policy(self, ok, cwd=None, **changes):
        env = dict(self.env, **changes)
        result = subprocess.run(["python3", str(POLICY)], cwd=cwd or self.repo, env=env,
                                capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, ok, result.stderr)
        output = self.output.read_text(encoding="utf-8")
        if not ok:
            self.assertEqual(output, "", "a rejected policy must not emit outputs")
        return output

    def dev(self, **changes):
        self.git("checkout", "--detach", self.feature)
        env = dict(GITHUB_SHA=self.feature, GITHUB_EVENT_NAME="workflow_dispatch",
                   GITHUB_REF="refs/heads/feature/test", GITHUB_REF_NAME="feature/test")
        env.update(changes)
        return env

    def test_stable_annotated_tag_and_lowercase_image(self):
        output = self.run_policy(True)
        self.assertEqual(output, "image=ghcr.io/example-org/ranking.test\n"
                         "tags<<GHCR_TAGS\n"
                         "ghcr.io/example-org/ranking.test:v2.3.4\n"
                         f"ghcr.io/example-org/ranking.test:sha-{self.main}\nGHCR_TAGS\n")

    def test_old_default_branch_commit_has_no_moving_aliases(self):
        self.git("checkout", "--detach", self.old)
        output = self.run_policy(True, GITHUB_SHA=self.old, GITHUB_REF="refs/tags/v0.0.0",
                                 GITHUB_REF_NAME="v0.0.0")
        self.assertIn(":v0.0.0\n", output)
        self.assertNotIn(":latest", output)
        self.assertNotIn(":0.0", output)

    def test_invalid_stable_tags_and_refs(self):
        for name in ("", "v1", "v1.2", "v1.2.3-rc.1", "v1.2.3+build", "v01.2.3",
                     "v1.02.3", "v1.2.03", "v-1.2.3", "v" + "1" * 128 + ".2.3",
                     "v1.2.3\ninjected=1", "v1.2.3$(touch nope)"):
            with self.subTest(name=name):
                self.run_policy(False, GITHUB_REF="refs/tags/" + name, GITHUB_REF_NAME=name)
        self.run_policy(False, GITHUB_REF="refs/heads/v2.3.4")
        self.run_policy(False, GITHUB_EVENT_NAME="pull_request")
        self.run_policy(False, DEV_TAG="feature")

    def test_unmerged_tag_wrong_event_and_missing_default_branch(self):
        self.git("checkout", "--detach", self.feature)
        self.run_policy(False, GITHUB_SHA=self.feature, GITHUB_REF="refs/tags/v2.3.5",
                        GITHUB_REF_NAME="v2.3.5")
        self.git("checkout", "--detach", self.main)
        self.run_policy(False, GITHUB_SHA=self.old)  # mismatched checkout/event
        self.run_policy(False, DEFAULT_BRANCH="missing")
        self.run_policy(False, DEFAULT_BRANCH="main\ninjected=1")
        self.run_policy(False, DEFAULT_BRANCH="")

    def test_moved_tag_is_rejected(self):
        self.git("update-ref", "refs/tags/v9.8.7", self.old)
        self.run_policy(False, GITHUB_REF="refs/tags/v9.8.7", GITHUB_REF_NAME="v9.8.7")

    def test_full_fetch_and_moved_remote_tag(self):
        origin = Path(self.temp.name) / "origin.git"
        checkout = Path(self.temp.name) / "checkout"
        def run(*args, cwd):
            subprocess.run(["git", *args], cwd=cwd, env=self.base_env, check=True,
                           capture_output=True, text=True)
        run("clone", "--bare", str(self.repo), str(origin), cwd=self.repo)
        checkout.mkdir()
        run("init", cwd=checkout)
        run("remote", "add", "origin", str(origin), cwd=checkout)
        def fetch():
            run("fetch", "--no-tags", "origin", "+refs/heads/*:refs/remotes/origin/*",
                "+refs/tags/*:refs/tags/*", cwd=checkout)
        fetch()
        run("checkout", "--detach", self.main, cwd=checkout)
        self.run_policy(True, cwd=checkout)
        self.output.write_text("", encoding="utf-8")
        run("--git-dir", str(origin), "update-ref", "refs/tags/v2.3.4", self.old, cwd=self.repo)
        fetch()
        self.run_policy(False, cwd=checkout)

    def test_development_namespace_and_default(self):
        output = self.run_policy(True, **self.dev())
        self.assertIn(f"ghcr.io/example-org/ranking.test:dev-sha-{self.feature}\n", output)
        self.assertEqual(output.count("ghcr.io/example-org/ranking.test:dev-"), 1)
        self.output.write_text("", encoding="utf-8")
        output = self.run_policy(True, **self.dev(DEV_TAG="feature-login_1.2"))
        self.assertIn(":dev-feature-login_1.2\n", output)
        self.assertNotIn(":latest", output)

    def test_development_rejects_reserved_malformed_and_injection(self):
        marker = Path(self.temp.name) / "should-not-exist"
        values = ("latest", "dev", "sha", "sha-" + self.main, "1", "1.2", "1.2.3",
                  "v1", "v1.2", "v1.2.3", "v01.2.3", "UPPER", "-leading", "a" * 125,
                  "name with spaces", "a/b", "a\ninjected=1", f"$(touch {marker})", "a;touch-pwned")
        for value in values:
            with self.subTest(value=value):
                self.run_policy(False, **self.dev(DEV_TAG=value))
        self.assertFalse(marker.exists())
        self.run_policy(False, **self.dev(GITHUB_REF="refs/heads/main", GITHUB_REF_NAME="main"))
        self.run_policy(False, **self.dev(GITHUB_REF="refs/tags/feature/test"))
        self.run_policy(False, **self.dev(GITHUB_REF_NAME="feature/test\ninjected=1"))

    def test_development_maximum_length(self):
        output = self.run_policy(True, **self.dev(DEV_TAG="a" * 124))
        self.assertIn(":dev-" + "a" * 124 + "\n", output)

    def test_invalid_repository_and_sha(self):
        for repository in ("", "owner", "owner/repo\ninjected=1", "owner/repo;cmd", "/repo",
                           "owner/repo/extra", "owner/.", "a" * 40 + "/repo", "owner/" + "a" * 101):
            with self.subTest(repository=repository):
                self.run_policy(False, GITHUB_REPOSITORY=repository)
        for sha in ("", "a" * 39, "A" * 40, "a" * 41, self.main + "\ninjected=1"):
            with self.subTest(sha=sha):
                self.run_policy(False, GITHUB_SHA=sha)


class CheckGateTest(unittest.TestCase):
    KEYS = ("CHANGES_RESULT", "BACKEND_RESULT", "FRONTEND_RESULT", "CONTAINER_RESULT")

    def run_gate(self, values):
        env = dict(os.environ)
        for key in ("FULL_CHECK", *self.KEYS):
            env.pop(key, None)
        env.update(values)
        return subprocess.run(["python3", str(GATE)], env=env, capture_output=True).returncode

    def test_full_check_requires_every_job_to_succeed(self):
        base = {"FULL_CHECK": "true", **dict.fromkeys(self.KEYS, "success")}
        self.assertEqual(self.run_gate(base), 0)
        for key in self.KEYS:
            for value in ("failure", "cancelled", "skipped", "", "unknown", "success\ninjected=1"):
                with self.subTest(key=key, value=value):
                    results = dict(base)
                    results[key] = value
                    self.assertNotEqual(self.run_gate(results), 0)

    def test_filtered_pull_request_allows_skipped_checks_only(self):
        base = {"FULL_CHECK": "false", **dict.fromkeys(self.KEYS, "success")}
        self.assertEqual(self.run_gate(base), 0)
        self.assertEqual(self.run_gate({
            "FULL_CHECK": "false",
            "CHANGES_RESULT": "success",
            "BACKEND_RESULT": "skipped",
            "FRONTEND_RESULT": "skipped",
            "CONTAINER_RESULT": "skipped",
        }), 0)
        self.assertEqual(self.run_gate({
            "FULL_CHECK": "false",
            "CHANGES_RESULT": "success",
            "BACKEND_RESULT": "success",
            "FRONTEND_RESULT": "skipped",
            "CONTAINER_RESULT": "skipped",
        }), 0)
        for bad_changes in ("failure", "cancelled", "skipped", "", "unknown", "success\ninjected=1"):
            with self.subTest(changes=bad_changes):
                results = dict(base, CHANGES_RESULT=bad_changes)
                self.assertNotEqual(self.run_gate(results), 0)
        for key in ("BACKEND_RESULT", "FRONTEND_RESULT", "CONTAINER_RESULT"):
            for value in ("failure", "cancelled", "", "unknown", "skipped\ninjected=1"):
                with self.subTest(key=key, value=value):
                    results = dict(base)
                    results[key] = value
                    self.assertNotEqual(self.run_gate(results), 0)

    def test_missing_or_malformed_full_check_fails_closed(self):
        valid_results = dict.fromkeys(self.KEYS, "success")
        self.assertNotEqual(self.run_gate(valid_results), 0)
        for bad_flag in ("", "1", "0", "True", "False", "yes", "true\ninjected=1", "false\ninjected=1"):
            with self.subTest(full_check=bad_flag):
                self.assertNotEqual(self.run_gate({**valid_results, "FULL_CHECK": bad_flag}), 0)


if __name__ == "__main__":
    unittest.main()
