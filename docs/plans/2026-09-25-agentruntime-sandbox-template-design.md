# AgentRuntime 沙箱模板设计

## 目标

基于 CubeSandbox 和 AgentENV 中可观察的工程实践，提供可复制的 AgentRuntime
沙箱仓库模板。

## 决策

- 模板不绑定 Runtime 语言。替换少量命令后，应能用于 Rust、Go 或混合语言实现。
- 提供运维约束而非复制上游产品代码：明确的生命周期状态、非 root 执行、
  宿主机能力检查、隔离的测试状态、失败清理和发布验证。
- 同时包含 `.claude` 和 `.agent` 约定。前者提供 Claude skill、评审 agent 与
  命令；后者提供任何 agent runner 都可使用的 Runtime 中立 skill 与工作流契约。
- 不随模板提供 MCP 配置。工具连接方式取决于部署，应由采用模板的项目自行评审。

## 模板布局

```text
templates/agentruntime-sandbox/
  .agent/                 与 Runtime 无关的 Agent skill 和工作流
  .claude/                Claude skill、agent 和命令
  .github/                CI、发布、Issue、PR 与归属策略
  scripts/sandbox/        宿主机检查、smoke、清理和模板验证
  config/                 脱敏的本地配置示例
  AGENTS.md / CLAUDE.md   贡献和编码契约
  Makefile                人员与 CI 共用的稳定入口
```

## 来源对应

| 模板区域 | CubeSandbox 借鉴点 | AgentENV 借鉴点 |
| --- | --- | --- |
| `.claude/agents`、命令、skill | 专项评审者和 `run-dev` | 项目指引与测试纪律 |
| `AGENTS.md` | AI 贡献署名和 DCO 策略 | 明确假设、保持简单和局部修改 |
| `.github` 治理 | DCO、CODEOWNERS、Issue 分流、组件工作流 | 丰富的 Issue 表单、PR 清单和安全策略 |
| 沙箱 CI 准备 | builder 与容器前置条件 | KVM、能力准备、依赖缓存和清理 |
| 生命周期工作流 | 沙箱与模板的运维约束 | 快照、模板和卷的生命周期术语 |

## 验证

`make template-check` 验证必需文件、JSON 设置、shell 语法，并确认未包含 MCP
模板。CI 通过 runner 的工作流解析器加载 GitHub YAML；维护者编辑工作流时还可
在本地使用 PyYAML。
