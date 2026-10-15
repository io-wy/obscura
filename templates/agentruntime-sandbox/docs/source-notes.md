# 来源记录

本模板是综合整理，而非逐文件复制。各部分参考了以下公开实践。

## CubeSandbox

- `.claude/agents/*-reviewer.md`：按质量、性能、测试、安全、文档及项目门禁
  拆分评审职责。
- `.claude/commands/review-pr.md`：组织专项评审，只发布值得关注的发现。
- `.claude/skills/run-dev/SKILL.md`：使用稳定的 Make 目标，记录构建和测试前置
  条件，提示 KVM、Redis、Docker 和 builder 常见问题。
- `AGENTS.md`：明确 AI 贡献署名，把 DCO 签署留给人类。
- `.github/workflows/dco-check.yml`、`code-review.yml` 和组件工作流：使用最小
  权限、路径过滤和并发取消，把昂贵检查与普通 CI 分开。
- `.github/CODEOWNERS` 和 Issue 模板：在贡献时呈现归属和复现背景。

## AgentENV

- `CLAUDE.md` / `AGENTS.md`：先阅读受影响模块，说明假设，采用简单局部改动，
  并为行为变化添加测试。
- `.github/actions/rust-ci-setup/action.yml`：把工具链和依赖准备封装为可复用
  action，缓存键由依赖输入决定。
- `.github/actions/sandbox-ci-setup/action.yml`：隔离状态与依赖路径，明确配置
  KVM 权限，准备前置条件，并在特权测试前检查宿主机能力。
- `.github/actions/sandbox-artifacts-cleanup/action.yml`：失败后清理泄漏进程并恢复
  工作区归属；本模板收窄为安全的本地工作区清理。
- 集成、基准、变异和发布工作流：显式运行高成本任务、收集产物，并根据包版本
  核验发布 tag。
- `SECURITY.md` 和结构化 Issue 表单：私下处理漏洞，在不收集 Secret 的前提下
  收集宿主机、存储、快照和性能信息。

## 有意省略

- 未复制上游 Runtime 实现、Docker 镜像、内核或存储驱动。这些选择取决于负载
  和威胁模型。
- 未提供 MCP 配置。工具凭据和网络出站需要采用者自行安全评审。
- 来自 fork 的 Pull Request 不会自动运行特权工作流。
