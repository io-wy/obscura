---
description: 验证当前改动并创建或复用 Obscura Pull Request
argument-hint: [改动范围] [是否准备好评审]
---

阅读 CONTRIBUTING.md 和 .github/pull_request_template.md。用户指定范围：$ARGUMENTS

1. 检查分支、工作区、相对 main 的 diff 和提交。若当前在 main，使用 branch 命令
   创建合适的 `<type>/<slug>` 分支，并保留本地改动。
2. 评审正确性，运行 CONTRIBUTING.md 中对应的验证。若指定范围内的改动还需提交
   和推送，按仓库 commit-push 命令流程操作，显式列出文件并使用 Conventional
   Commit 标题。调用 `/pr` 授权为指定工作做这些准备并创建 PR。保留无关工作；
   PR 脚本要求干净的工作区，因此若无关待处理改动阻止完成，应报告原因，不得把它们
   混入 PR 或悄悄 stash。
3. 使用 Conventional Commits 格式拟定具体 PR 标题。按照模板把行为、Runtime
   影响、实际检查、限制和评审重点写入工作区外的临时文件；保留真实换行并使用
   `--body-file`。
4. 执行 `python3 scripts/git-workflow.py pr --title '<标题>' --body-file <文件>`。
   脚本创建草稿 PR，或返回相同 origin/head/main 已开放的 PR。仅当用户要求准备好
   评审的 PR 时添加 `--ready`；不改写已有 PR。不合并、不改保护规则、不强推、
   不发布 tag。
5. 删除临时正文文件。若宿主环境提供 PR 附件工具，将返回的 PR URL 关联到任务。
   报告 URL、简要摘要和实际验证结果。

安全传参，不得把用户文本直接插入 shell 代码。
