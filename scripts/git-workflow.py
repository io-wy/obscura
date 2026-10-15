#!/usr/bin/env python3
"""Small, dependency-free GitHub Flow commands. Requires Python 3.11+."""
import argparse
import json
import os
from pathlib import Path
import re
import subprocess
import sys


TYPES = "feat fix docs refactor perf test build ci chore revert".split()
TYPE_PATTERN = "(?:" + "|".join(TYPES) + ")"
BRANCH = re.compile(r"(?:codex/)?" + TYPE_PATTERN + r"/[a-z0-9]+(?:-[a-z0-9]+)*")
MESSAGE = re.compile(TYPE_PATTERN + r"(?:\([a-z0-9]+(?:[-/][a-z0-9]+)*\))?!?: \S(?:.*\S)?")
NUMBER = r"(?:0|[1-9][0-9]*)"
VERSION = re.compile(NUMBER + r"\." + NUMBER + r"\." + NUMBER + r"(?:-rc\.[1-9][0-9]*)?")


class WorkflowError(Exception):
    pass


def run(*args, capture=False):
    result = subprocess.run(args, text=True, capture_output=capture, check=True)
    return result.stdout.strip() if capture else ""


def git(*args, capture=True):
    return run("git", *args, capture=capture)


def require(condition, message):
    if not condition:
        raise WorkflowError(message)


def check_message(message):
    require(MESSAGE.fullmatch(message) is not None,
            "Use Conventional Commits, e.g. feat(runtime): add sandbox lifecycle")
    require(len(message) <= 100, "Commit/PR subject must be at most 100 characters.")


def check_branch(branch):
    require(BRANCH.fullmatch(branch) is not None,
            "Use [codex/]<type>/<lowercase-slug>, e.g. codex/feat/runtime-adapter")


def topic_branch():
    branch = git("branch", "--show-current")
    check_branch(branch)  # Also rejects main and detached HEAD.
    return branch


def require_clean():
    require(not git("status", "--porcelain"), "Working tree must be clean (including untracked files).")


def remote_head(branch):
    result = git("ls-remote", "--heads", "origin", "refs/heads/" + branch)
    return result.split()[0] if result else ""


def fetch_main():
    git("fetch", "--no-tags", "origin", "refs/heads/main", capture=False)
    return git("rev-parse", "FETCH_HEAD")


def push(branch):
    # Explicit refspec prevents push.default/matching from pushing other branches.
    git("push", "--no-follow-tags", "--set-upstream", "origin",
        "HEAD:refs/heads/" + branch, capture=False)


def check_version(tag=None):
    version = Path("VERSION").read_text().strip()
    require(VERSION.fullmatch(version) is not None, "VERSION must be X.Y.Z or X.Y.Z-rc.N.")
    if tag:
        require(tag == "v" + version, "Tag must match VERSION: v" + version)
    # These packages currently ship together. Missing packages allow bootstrap repos.
    cargo = Path("runtime/crates/runtime/Cargo.toml")
    if cargo.exists():
        import tomllib
        require(tomllib.loads(cargo.read_text())["package"]["version"] == version,
                "Rust package version differs from VERSION.")
    for name in ("guest/runner/package.json", "guest/runner/package-lock.json"):
        path = Path(name)
        if path.exists():
            package = json.loads(path.read_text())
            require(package["version"] == version, name + " differs from VERSION.")
            if "packages" in package:
                require(package["packages"][""]["version"] == version,
                        name + " root package differs from VERSION.")
    return version


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    commands = parser.add_subparsers(dest="command", required=True)
    branch = commands.add_parser("branch", help="Create and switch to codex/<type>/<slug>")
    branch.add_argument("type", choices=TYPES)
    branch.add_argument("slug")
    branch.add_argument("--base", default="HEAD", help="Starting ref; defaults to current HEAD")
    commit = commands.add_parser("commit-push", help="Commit staged/explicit paths and push this branch")
    commit.add_argument("-m", "--message", required=True)
    commit.add_argument("--paths", nargs="+", help="Explicit paths; requires an empty index")
    commands.add_parser("push", help="Retry pushing the current topic branch")
    pr = commands.add_parser("pr", help="Create a draft PR against main, or return an existing PR")
    pr.add_argument("--title", required=True)
    pr.add_argument("--body-file", type=Path, required=True)
    pr.add_argument("--ready", action="store_true", help="Create ready for review instead of draft")
    tag = commands.add_parser("tag", help="Create an annotated version tag on published main")
    tag.add_argument("name")
    tag.add_argument("--push", action="store_true", help="Also publish this one tag")
    commands.add_parser("install-hooks", help="Opt in to the repository commit-msg hook")
    message = commands.add_parser("check-message", help="Validate a commit/PR subject")
    message.add_argument("message")
    message_file = commands.add_parser("check-message-file", help="Validate the first line of a commit message file")
    message_file.add_argument("path", type=Path)
    check = commands.add_parser("check-branch")
    check.add_argument("name")
    version = commands.add_parser("check-version")
    version.add_argument("--tag")
    args = parser.parse_args()

    # Pure validators do not require a repository (also used for untrusted PR titles).
    if args.command == "check-message":
        check_message(args.message)
        return
    if args.command == "check-message-file":
        lines = args.path.read_text().splitlines()
        check_message(lines[0] if lines else "")
        return
    if args.command == "check-branch":
        check_branch(args.name)
        return

    os.chdir(git("rev-parse", "--show-toplevel"))
    if args.command == "branch":
        name = "codex/" + args.type + "/" + args.slug
        check_branch(name)
        # Resolve first so a ref supplied by the caller cannot become a switch flag.
        base = git("rev-parse", "--verify", "--end-of-options", args.base + "^{commit}")
        git("switch", "-c", name, base, capture=False)
    elif args.command == "commit-push":
        branch = topic_branch()
        check_message(args.message)
        git("remote", "get-url", "origin")
        if args.paths:
            require(not git("diff", "--cached", "--name-only"),
                    "Index already contains changes; review it and use staged-only mode, or unstage first.")
            git("--literal-pathspecs", "add", "-A", "--", *args.paths, capture=False)
        require(bool(git("diff", "--cached", "--name-only")), "No staged changes to commit.")
        git("diff", "--cached", "--check", capture=False)
        git("diff", "--cached", "--stat", capture=False)
        git("commit", "-m", args.message, capture=False)
        print("Commit created. If push fails, retry with: python3 scripts/git-workflow.py push", flush=True)
        push(branch)
    elif args.command == "push":
        push(topic_branch())
    elif args.command == "pr":
        branch = topic_branch()
        check_message(args.title)
        body = args.body_file.resolve()
        require(body.is_file() and bool(body.read_text().strip()), "Provide a nonempty PR body file.")
        require_clean()
        require(remote_head(branch) == git("rev-parse", "HEAD"),
                "Push this branch before creating a PR (run the push command).")
        base = fetch_main()
        require(int(git("rev-list", "--count", base + "..HEAD")) > 0,
                "Branch has no commits beyond origin/main.")
        repository = git("remote", "get-url", "origin")
        existing = json.loads(run("gh", "pr", "list", "--repo", repository,
                                  "--head", branch, "--base", "main", "--state", "open",
                                  "--json", "url,isCrossRepository", capture=True))
        existing = [pr for pr in existing if not pr.get("isCrossRepository", False)]
        if existing:
            print(existing[0]["url"])
            return
        flags = [] if args.ready else ["--draft"]
        run("gh", "pr", "create", "--repo", repository, "--head", branch, "--base", "main",
            "--title", args.title, "--body-file", str(body), *flags)
    elif args.command == "tag":
        check_version(args.name)
        require(git("branch", "--show-current") == "main", "Create release tags from main.")
        require_clean()
        require(git("rev-parse", "HEAD") == fetch_main(), "Local main must equal published origin/main.")
        require(not git("ls-remote", "--tags", "origin", "refs/tags/" + args.name),
                "Tag already exists on origin; published tags are immutable.")
        local = git("tag", "--list", args.name)
        if local:
            require(args.push and git("cat-file", "-t", args.name) == "tag"
                    and git("rev-parse", args.name + "^{commit}") == git("rev-parse", "HEAD"),
                    "Local tag exists; only --push of an annotated tag on this HEAD is allowed.")
        else:
            git("tag", "-a", args.name, "-m", "Release " + args.name, capture=False)
        if args.push:
            git("push", "--no-follow-tags", "origin", "refs/tags/" + args.name, capture=False)
    elif args.command == "check-version":
        check_version(args.tag)
    elif args.command == "install-hooks":
        configured = subprocess.run(["git", "config", "--get", "core.hooksPath"],
                                    text=True, capture_output=True)
        require(configured.returncode in (0, 1), "Cannot read core.hooksPath.")
        require(configured.stdout.strip() in ("", ".githooks"),
                "Existing core.hooksPath detected; integrate commit-msg manually.")
        old_hooks = Path(git("rev-parse", "--git-path", "hooks"))
        installed = [hook for hook in old_hooks.glob("*")
                     if hook.is_file() and not hook.name.endswith(".sample")]
        require(not installed or configured.stdout.strip() == ".githooks",
                "Existing Git hooks detected; integrate manually before enabling .githooks.")
        git("config", "--local", "core.hooksPath", ".githooks", capture=False)
        print("Enabled .githooks/commit-msg for this checkout.")


if __name__ == "__main__":
    try:
        main()
    except (WorkflowError, OSError, ValueError, KeyError, subprocess.CalledProcessError) as error:
        if isinstance(error, subprocess.CalledProcessError) and error.stderr:
            print(error.stderr.rstrip(), file=sys.stderr)
        print("error:", error, file=sys.stderr)
        sys.exit(1)
