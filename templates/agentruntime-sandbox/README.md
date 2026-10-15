# AgentRuntime 沙箱模板

这是 AgentRuntime 沙箱仓库的精简起点，从以下项目提炼工程实践：

- [TencentCloud/CubeSandbox](https://github.com/TencentCloud/CubeSandbox)
- [kvcache-ai/AgentENV](https://github.com/kvcache-ai/AgentENV)

这些文件是原创、与 Runtime 实现无关的示例。启用必需 CI 前，先把
`TODO(adopter)` 标记的命令替换为本项目实际命令。

## 包含内容

| 区域 | 用途 |
| --- | --- |
| `AGENTS.md`、`CLAUDE.md` | 共同的编码和贡献契约 |
| `.claude/` | 开发 skill、专项评审 agent 和 `/review-pr` |
| `.agent/` | 与 Runtime 无关的 skill、生命周期及发布工作流 |
| `.github/` | 最小权限 CI、Issue/PR 表单、CODEOWNERS、发布骨架 |
| `scripts/sandbox/` | 宿主机检查、smoke 入口、清理和模板验证 |
| `config/` | 脱敏的本地配置示例 |
| `Makefile` | 人员与 CI 共用的稳定命令 |

## 用于其他仓库

1. 将本目录完整复制到目标仓库根目录，包括隐藏文件。
2. 替换 `Makefile` 和 `scripts/sandbox/` 中的 `TODO(adopter)` 命令。
3. 在 `.github/CODEOWNERS` 中填写真正的负责人。
4. 根据 Runtime 选择宿主机检查：KVM、容器 Runtime 或其他隔离后端。
5. 本地运行 `make template-check`。
6. 只在可信且具备所需宿主机能力的 runner 上启用 `sandbox-integration.yml`。

## 安全默认值

- Pull Request 只运行 `contents: read` 的只读检查。
- 集成测试默认手动触发，绝不通过 `pull_request_target` 在特权 runner 上运行
  贡献者代码。
- 测试使用专用状态目录，不指向在线沙箱服务或生产对象存储。
- 清理尽力执行，但范围只限配置的工作区和状态路径。
- Issue 模板、示例配置和 Agent 指令中不放置 Secret 或凭据。

## 生命周期契约

Runtime 至少应暴露这些可观察状态：

`Creating -> Ready -> Running -> Paused -> Deleted`

任何失败转换都必须记录原因并释放临时宿主机资源。快照或模板发布后不可变；
从中启动的新沙箱必须得到新身份。即使底层使用容器而非 microVM，也应保持该契约。

## 来源说明

本模板独立综合公开的项目约定，不是 CubeSandbox 或 AgentENV 的官方发行版。
