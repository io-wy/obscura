"""Exercise Git mutations against disposable repositories and a local remote."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import sys
import tempfile
import unittest


SCRIPT = Path(__file__).resolve().parents[1] / "git-workflow.py"


class WorkflowTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.repo = self.root / "repo"
        self.repo.mkdir()
        self.git("init", "-b", "main")
        self.git("config", "user.email", "test@example.invalid")
        self.git("config", "user.name", "Test")
        (self.repo / "VERSION").write_text("0.1.0\n")
        self.git("add", "VERSION")
        self.git("commit", "-m", "chore: initialize")
        subprocess.run(["git", "init", "--bare", str(self.root / "remote.git")],
                       check=True, capture_output=True)
        self.git("remote", "add", "origin", str(self.root / "remote.git"))
        self.git("push", "-u", "origin", "main")
        self.env = dict(os.environ)

    def git(self, *args):
        return subprocess.run(["git", *args], cwd=self.repo, check=True,
                              capture_output=True, text=True).stdout.strip()

    def run_cli(self, *args, ok=True):
        result = subprocess.run([sys.executable, str(SCRIPT), *args], cwd=self.repo,
                                env=self.env, capture_output=True, text=True)
        self.assertEqual(result.returncode == 0, ok, result.stdout + result.stderr)
        return result

    def branch(self):
        self.run_cli("branch", "feat", "example")

    def test_branch_validation_and_dirty_work_preserved(self):
        (self.repo / "pending").write_text("keep me")
        self.run_cli("branch", "feat", "Bad Name", ok=False)
        self.run_cli("branch", "feat", "example", "--base=--discard-changes", ok=False)
        self.branch()
        self.assertEqual(self.git("branch", "--show-current"), "feat/example")
        self.assertEqual((self.repo / "pending").read_text(), "keep me")

    def test_old_branch_prefix_is_rejected(self):
        self.run_cli("check-branch", "feat/example")
        self.run_cli("check-branch", "codex/feat/example", ok=False)

    def test_protected_branch_and_invalid_message_refuse_mutation(self):
        before = self.git("rev-parse", "HEAD")
        self.run_cli("commit-push", "-m", "feat: example", ok=False)
        self.branch()
        (self.repo / "a").write_text("a")
        self.run_cli("commit-push", "-m", "bad message", "--paths", "a", ok=False)
        self.assertEqual(self.git("rev-parse", "HEAD"), before)
        self.assertEqual(self.git("diff", "--cached", "--name-only"), "")

    def test_only_explicit_paths_committed_and_pushed(self):
        self.branch()
        (self.repo / "chosen").write_text("chosen")
        (self.repo / "unrelated").write_text("unrelated")
        self.run_cli("commit-push", "-m", "feat(runtime): add example",
                     "--paths", "chosen")
        self.assertEqual(self.git("show", "--pretty=", "--name-only", "HEAD"), "chosen")
        self.assertIn("?? unrelated", self.git("status", "--short"))
        self.assertEqual(self.git("rev-parse", "HEAD"),
                         self.git("rev-parse", "origin/feat/example"))

    def test_staged_only_and_refuse_mixing_paths(self):
        self.branch()
        self.run_cli("commit-push", "-m", "fix: empty", ok=False)
        (self.repo / "a").write_text("a")
        (self.repo / "b").write_text("b")
        self.git("add", "a")
        self.run_cli("commit-push", "-m", "fix: example", "--paths", "b", ok=False)
        self.assertEqual(self.git("diff", "--cached", "--name-only"), "a")
        self.run_cli("commit-push", "-m", "fix: example")
        self.assertEqual(self.git("show", "--pretty=", "--name-only", "HEAD"), "a")

    def test_tag_requires_clean_main_and_matching_version(self):
        self.run_cli("tag", "v0.2.0", ok=False)
        self.branch()
        self.run_cli("tag", "v0.1.0", ok=False)
        self.git("switch", "main")
        (self.repo / "pending").write_text("pending")
        self.run_cli("tag", "v0.1.0", ok=False)
        (self.repo / "pending").unlink()
        self.run_cli("tag", "v0.1.0")
        self.assertEqual(self.git("cat-file", "-t", "v0.1.0"), "tag")
        self.assertEqual(self.git("ls-remote", "--tags", "origin"), "")

    def fake_gh(self, existing):
        executable = self.root / "gh"
        executable.write_text(
            "#!/usr/bin/env python3\nimport json, os, sys\n"
            "from pathlib import Path\n"
            "with open(os.environ['GH_LOG'], 'a') as f: f.write(json.dumps(sys.argv[1:]) + '\\n')\n"
            "if sys.argv[1:3] == ['pr', 'list']:\n"
            "    print(os.environ['GH_EXISTING'])\n"
            "else:\n"
            "    p = sys.argv.index('--body-file')\n"
            "    Path(os.environ['GH_BODY']).write_text(Path(sys.argv[p+1]).read_text())\n"
            "    print('https://github.com/example/repo/pull/2')\n"
        )
        executable.chmod(0o755)
        self.env.update(PATH=str(self.root) + os.pathsep + self.env["PATH"],
                        GH_LOG=str(self.root / "gh.log"),
                        GH_BODY=str(self.root / "body.copy"),
                        GH_EXISTING=json.dumps(existing))

    def test_pr_creates_draft_with_literal_body_and_reuses_existing(self):
        self.branch()
        (self.repo / "a").write_text("a")
        self.run_cli("commit-push", "-m", "feat: example", "--paths", "a")
        body = self.root / "body.md"
        body.write_text("Summary\n\n`literal` $(literal)\n")
        self.fake_gh([])
        self.run_cli("pr", "--title", "feat: example", "--body-file", str(body))
        calls = [json.loads(line) for line in (self.root / "gh.log").read_text().splitlines()]
        self.assertIn("--draft", calls[-1])
        self.assertIn("--head", calls[-1])
        self.assertEqual((self.root / "body.copy").read_text(), body.read_text())
        self.fake_gh([{"url": "https://github.com/example/repo/pull/2"}])
        self.run_cli("pr", "--title", "feat: example", "--body-file", str(body))
        calls = [json.loads(line) for line in (self.root / "gh.log").read_text().splitlines()]
        self.assertEqual(sum(call[:2] == ["pr", "create"] for call in calls), 1)

    def test_pr_refuses_dirty_or_unpushed_branch(self):
        self.branch()
        body = self.root / "body.md"
        body.write_text("Summary")
        self.fake_gh([])
        (self.repo / "a").write_text("a")
        self.git("add", "a")
        self.git("commit", "-m", "feat: example")
        self.run_cli("pr", "--title", "feat: example", "--body-file", str(body), ok=False)
        self.run_cli("push")
        (self.repo / "pending").write_text("pending")
        self.run_cli("pr", "--title", "feat: example", "--body-file", str(body), ok=False)
        self.assertFalse((self.root / "gh.log").exists())

    def test_failed_push_keeps_commit_and_can_retry(self):
        self.branch()
        reject = self.root / "remote.git/hooks/pre-receive"
        reject.write_text("#!/bin/sh\nexit 1\n")
        reject.chmod(0o755)
        (self.repo / "a").write_text("a")
        before = self.git("rev-parse", "HEAD")
        self.run_cli("commit-push", "-m", "feat: example", "--paths", "a", ok=False)
        after = self.git("rev-parse", "HEAD")
        self.assertNotEqual(before, after)
        self.assertEqual(self.git("status", "--porcelain"), "")
        reject.unlink()
        self.run_cli("push")
        self.assertEqual(self.git("rev-parse", "origin/feat/example"), after)

    def test_tag_push_retry_publishes_only_selected_tag(self):
        self.git("tag", "unrelated")
        self.run_cli("tag", "v0.1.0")
        self.run_cli("tag", "v0.1.0", "--push")
        tags = self.git("ls-remote", "--tags", "origin")
        self.assertIn("refs/tags/v0.1.0", tags)
        self.assertNotIn("unrelated", tags)
        self.run_cli("tag", "v0.1.0", "--push", ok=False)

    def test_tag_refuses_unpublished_main(self):
        (self.repo / "a").write_text("a")
        self.git("add", "a")
        self.git("commit", "-m", "feat: unpublished")
        self.run_cli("tag", "v0.1.0", ok=False)
        self.assertEqual(self.git("tag", "--list"), "")

    def test_package_version_consistency(self):
        guest = self.repo / "guest/runner"
        guest.mkdir(parents=True)
        (guest / "package.json").write_text('{"version": "0.2.0"}')
        self.run_cli("check-version", ok=False)
        (guest / "package.json").write_text('{"version": "0.1.0"}')
        self.run_cli("check-version", "--tag", "v0.1.0")
        self.run_cli("check-version", "--tag", "v00.1.0", ok=False)

    def test_hook_checks_real_commits_without_overwriting_existing_setup(self):
        hook_dir = self.repo / ".githooks"
        hook_dir.mkdir()
        shutil.copy2(SCRIPT.parents[1] / ".githooks/commit-msg", hook_dir / "commit-msg")
        scripts = self.repo / "scripts"
        scripts.mkdir()
        shutil.copy2(SCRIPT, scripts / SCRIPT.name)
        self.git("config", "core.hooksPath", "/some/existing/hooks")
        self.run_cli("install-hooks", ok=False)
        self.assertEqual(self.git("config", "--get", "core.hooksPath"), "/some/existing/hooks")
        self.git("config", "--unset", "core.hooksPath")
        old_hook = self.repo / ".git/hooks/pre-commit"
        old_hook.write_text("#!/bin/sh\nexit 0\n")
        self.run_cli("install-hooks", ok=False)
        self.assertTrue(old_hook.exists())
        old_hook.unlink()
        self.run_cli("install-hooks")
        self.git("add", ".githooks", "scripts")
        result = subprocess.run(["git", "commit", "-m", "invalid"], cwd=self.repo,
                                capture_output=True, text=True)
        self.assertNotEqual(result.returncode, 0)
        self.assertIn("Conventional Commits", result.stderr)
        self.git("commit", "-m", "chore(repo): install hook\n\nCommit body is allowed.")

    def test_message_validation(self):
        for message in ["feat: add controller", "fix(runtime)!: change API", "docs: 更新说明"]:
            self.run_cli("check-message", message)
        for message in ["update things", "feat: ", "feat(): missing scope", "feat: a\nfix: b"]:
            self.run_cli("check-message", message, ok=False)


if __name__ == "__main__":
    unittest.main()
