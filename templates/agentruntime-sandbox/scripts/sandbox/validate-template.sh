#!/usr/bin/env bash
set -euo pipefail

root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
required=(
  README.md AGENTS.md CLAUDE.md Makefile SECURITY.md docs/source-notes.md config/sandbox.env.example
  .gitignore SECURITY.md
  .claude/settings.json .claude/commands/review-pr.md
  .claude/skills/sandbox-dev/SKILL.md .claude/skills/runtime-review/SKILL.md
  .claude/agents/security-reviewer.md .claude/agents/test-reviewer.md
  .agent/README.md .agent/skills/sandbox-contract/SKILL.md
  .agent/skills/ci-triage/SKILL.md .agent/workflows/pr-review.md
  .agent/workflows/sandbox-lifecycle.md .agent/workflows/release.md
  .github/CODEOWNERS .github/pull_request_template.md
  .github/ISSUE_TEMPLATE/config.yml .github/ISSUE_TEMPLATE/bug.yml
  .github/ISSUE_TEMPLATE/feature.yml .github/ISSUE_TEMPLATE/performance.yml
  .github/workflows/ci.yml .github/workflows/sandbox-integration.yml
  .github/workflows/security.yml .github/workflows/release.yml
  .github/actions/sandbox-ci-setup/action.yml
  scripts/sandbox/check-host.sh scripts/sandbox/smoke-test.sh
  scripts/sandbox/cleanup.sh scripts/sandbox/validate-template.sh
)

for rel in "${required[@]}"; do
  test -f "$root/$rel" || { echo "missing: $rel" >&2; exit 1; }
done

python3 - "$root/.claude/settings.json" <<'PY'
import json
import sys

with open(sys.argv[1], encoding="utf-8") as fh:
    data = json.load(fh)
assert isinstance(data.get("permissions", {}).get("deny"), list)
PY

while IFS= read -r script; do
  bash -n "$script"
done < <(find "$root/scripts" -type f -name '*.sh' -print)

if find "$root" -iname '*mcp*' -print -quit | grep -q .; then
  echo 'MCP files are intentionally excluded from this template.' >&2
  exit 1
fi

echo "AgentRuntime sandbox template is valid: $root"
