---
description: 评审并验证指定改动，按 Conventional Commits 提交和推送
argument-hint: [提交范围或文件]
---

阅读 CONTRIBUTING.md。用户指定范围：$ARGUMENTS

1. 检查 `git status --short`、分支、完整暂存 diff，以及相关未暂存或未跟踪改动。
   确定用户指定范围并保留无关工作。不要默认使用 `git add .` 或 `git add -A`。
2. 使用符合 `<type>/<slug>` 的主题分支。若当前在 main，提交前先通过
   branch 命令从当前 HEAD 创建 `<type>/<slug>` 分支。
3. 评审实际补丁的正确性和敏感信息。按 CONTRIBUTING.md 对受影响模块运行检查，
   报告无法运行的具体检查；不得声称未运行的检查通过。
4. 选择准确的 Conventional Commit 标题。如果暂存区为空，执行
   `python3 scripts/git-workflow.py commit-push -m '<title>' --paths <明确的路径>`。
   若已评审的暂存改动恰好属于目标范围，可省略 `--paths`。若暂存区含无关改动，
   先解决范围问题；不得悄悄取消他人的暂存。
5. 本命令授权提交选定改动，并把当前主题分支推送到 origin。不得强推、修改已发布
   提交、推送 main 或 tag，也不得绕过 hook 或检查。提交成功而推送失败时，使用
   `python3 scripts/git-workflow.py push` 重试。
6. 报告分支、commit SHA、验证结果和推送结果。除非用户要求，不创建 PR。

把参数作为独立进程参数传入，或正确进行 shell 引用；不得把不可信文本插入 shell 代码。
