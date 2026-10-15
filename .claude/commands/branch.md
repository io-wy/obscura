---
description: 按仓库命名规范创建 Obscura 主题分支
argument-hint: <type> <slug> [--base <ref>]
---

阅读 CONTRIBUTING.md，检查当前分支和工作区。用户参数：$ARGUMENTS

使用 `python3 scripts/git-workflow.py branch <type> <slug>` 创建并切换分支。
结果为 `<type>/<slug>`；除非明确指定 base，否则从当前 HEAD 开始。
尽可能根据任务推断简短的小写 slug。保留现有改动。创建分支时不要 reset、stash、
fetch、commit 或 push。报告新分支和起始 ref。
