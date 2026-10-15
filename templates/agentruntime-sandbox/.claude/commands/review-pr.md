---
description: 评审当前 Pull Request 的 Runtime 和沙箱风险
---

执行以下专项评审，并合并可采取行动的发现：

- `.claude/agents/security-reviewer.md`
- `.claude/agents/test-reviewer.md`
- `.claude/skills/runtime-review/SKILL.md`

检查 PR diff、相关测试和工作流变化。按严重程度报告发现，并附文件与行号。
确认特权集成测试仍为手动触发，或只在可信 runner 上隔离运行。除非用户明确要求，
不要发表评论或推送改动。
