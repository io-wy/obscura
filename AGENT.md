# 仓库协作指引

## 文档语言

- 新增和修改的项目说明、设计、计划、进度、验证记录与交接文档使用中文。
- 协议字段、代码标识符、命令、上游项目名称和引用保持原样。
- 历史英文文档在相关内容修改时同步更新中文说明，不把过期计划当作实现状态。

## 推送 dev 前的验证

- 每次推送 `dev` 前，必须在最终待推送代码上完整执行一次 `make ci`，确认退出码为 0。
- 首次或依赖变化后先执行 `make dev-bootstrap`；需要 Go、Rust、Node、Python、protoc 和 kubectl。
- `make ci` 与 GitHub Actions 共用入口，包含 Go/Rust/guest/Git 工具测试、真实 API Server 控制面联调、Go 竞态、清单/RBAC/schema、控制面构建、版本及 diff 检查。
- 提交标题遵循 CONTRIBUTING.md；远端 Git conventions 按事件分别检查 PR 标题和 push 提交标题，不能对 GitHub 自动生成的 PR 合并提交套用业务提交标题规则。
- 检查失败时先定位并修复，再重新执行完整检查。不能用跳过测试、忽略退出码或旧结果代替本轮验证。
- 本地通过后再推送；随后检查本次提交的远端 CI，区分本地通过、远端运行中和远端通过，不能提前声称完成。
- envtest 默认允许每个控制面进程在 90 秒内退出，保留 Stop 错误；可用 `KUBEBUILDER_CONTROLPLANE_STOP_TIMEOUT` 显式覆盖。发生超时要检查日志，不能直接忽略清理失败。

## 实现与验收边界

- 按 [当前工作进度](docs/progress.md) 和实际代码核对交付状态。
- 协议模拟通过不等于真实沙箱验收；Linux/KVM、AgentENV 等上游部署、隔离和资源回收必须单独记录。
- Provider 未通过固定版本的部署验收前，不开放 Registry，不提前宣称支持能力。
- 保留用户已有修改；不强推、不擅自删除共享分支或改动远端保护规则。
