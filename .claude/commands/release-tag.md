---
description: 验证并创建 Obscura 附注版本 tag，仅在用户要求时发布
argument-hint: vX.Y.Z[-rc.N] [--push]
---

阅读 CONTRIBUTING.md。用户参数：$ARGUMENTS

检查 VERSION、各包版本、当前分支和状态，以及发布提交的 CI 结果。确认所需版本更新
已经评审并合并，且完成适当的发布验证。不要为了让 tag 命令通过而在 main 上改写包版本。

依次运行 `python3 scripts/git-workflow.py check-version --tag <tag>` 和
`python3 scripts/git-workflow.py tag <tag>`。脚本要求 main 干净且与 origin/main 一致。
仅当用户明确要求发布或推送 tag（包括显式传入 --push）时添加 `--push`；默认只创建
本地 tag。不得移动或删除已发布的 tag。报告 tag、commit SHA 和发布状态。
不要部署或创建 GitHub Release。
