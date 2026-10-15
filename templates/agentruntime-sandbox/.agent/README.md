# 与 Runtime 无关的 Agent 契约

`.agent` 目录刻意不依赖 Claude。AgentRuntime 工具层无需绑定特定模型或 MCP
集成，即可加载这些 Markdown skill 和工作流契约。

- `skills/` 包含可重复执行的工程流程。
- `workflows/` 包含评审、生命周期和发布清单。

这里有意不提供 MCP 配置。每个部署都应自行选择并审计工具权限、凭据和网络出站。
