# 当前工作进度

更新日期：2026-10-16。本文按 `dev` 的实际代码和验证记录核对，历史计划不代表完成状态。

## 当前结论

控制面和自研 Runtime 的第一批代码已汇入 `dev`，能够在 macOS 上通过真实
Kubernetes API Server、Controller、Gateway 和模拟 Runtime 做链路验证。
目前仍处于协议与本地集成阶段，尚未完成真实 Linux/KVM 或任何外部 Provider 的部署准入。
“至少接入四个完整沙箱 Runtime”的目标还未达成，不能按 profile 数量计为已接入。

## 已实现的内容与边界

| 模块 | 已实现 | 未完成或尚未验证 |
| --- | --- | --- |
| Controller | Sandbox/Runtime/Tenant CRD、不可变规格、固定绑定、创建意图、状态观察、TTL、finalizer 和删除确认 | 跨节点原生调度、真实 VM 故障与资源泄漏验收 |
| 租户与配额 | Secret 摘要鉴权、轮换/撤销、独占命名空间、资源预留、执行/PTY 租约、清理确认后释放 | 实际集群 ServiceAccount 授权与多副本部署验收；Tenant 内细粒度用户权限 |
| Gateway | 异步创建/查询/删除、NDJSON 执行、GET/PUT 文件、WebSocket PTY、统一错误与大小/超时限制 | HTTP 端口代理路由尚未接入；真实 Provider 数据面验收 |
| 共享 HTTP adapter | 生命周期发现/分页/歧义处理，执行/文件/PTY 以及端口代理接口，provider profile/codec 边界 | 通用测试协议不能替代各 Provider 的真实接口；端口代理尚无公开 Gateway 路由 |
| 自研 Runtime | Firecracker 生命周期代码、SQLite v2、实际镜像校验、episode/generation、幂等 CLEAN reset、旧执行流取消、网络/磁盘清理 | Linux/KVM、vsock、conntrack、隔离与重启后的实际资源归属验收 |
| RL 轨迹与评分边界 | 有界轨迹元数据日志、摘要/校验/恢复、宿主评分票据和来源校验 | 原始轨迹内容回放、远端导出、轮转、独立 scorer worker、跨重启票据去重 |
| 高吞吐 Runtime | 已有设计和模块边界 | containerd/runc、runsc、预热池调度、性能与成本基准尚未实现 |

自研节点不提供独立的最大生命周期保证：TTL 由 Controller 到期删除，adapter
不发送节点尚未支持的 `max_runtime_nanos`。域名白名单、持久工作区、凭证代理、
CHECKPOINT/TEMPLATE reset 以及未接入的版本内容校验会明确拒绝，不能当成已生效策略。

## Provider 实际接入状态

| Runtime | 固定版本 | 当前状态 | 还缺什么 |
| --- | --- | --- | --- |
| Obscura native | 当前仓库 `0.1.0` | gRPC/UDS adapter 与 Rust node 已实现；本地联调使用模拟 node | 真实 Linux/KVM 完整生命周期与执行验收 |
| AgentENV | `v0.2.3` | 生命周期与 Connect execute、stdin、multipart/raw 文件契约测试通过；Registry 仍拒绝启用 | 固定版本实机部署；取消后远端进程确实终止；执行、文件及资源清理验收 |
| OpenSandbox | `release-1.1.1` | 版本和接口差异已记录；Registry 拒绝启用 | 专用生命周期/响应 codec、execd 数据面、契约测试及部署准入 |
| CubeSandbox | `v0.7.3` | 版本和 Cube envelope/envd 条件已记录；Registry 拒绝启用 | 专用 codec、数据面路由与鉴权、契约测试及部署准入 |
| E2B-shaped profile | 兼容性原型，未固定上游发布 | 可在 Registry 打开，用于共享 HTTP 契约开发 | 不能视为已通过某个真实 E2B 服务或上述 Provider 的验收 |

AgentENV 的 PTY、端口、快照和 clone 没有实现到专用 adapter，也没有对外声明支持。
目前固定了三个外部候选；若目标是四个外部完整沙箱，需要再选一个提供远程 API 的项目。
只有 CRD/operator 入口的项目仍不纳入本轮 Provider 接入范围。

## 当前 CI 与验证要求

统一入口为 `make ci`，本地推送 `dev` 前完整执行，GitHub Actions 使用相同命令。
它涵盖 Go、Rust、guest、Git 工具、envtest 控制面、Go race、部署清单/RBAC/schema、
控制面构建、版本与 diff 检查。首次或依赖变化时先运行 `make dev-bootstrap`。
提交/PR 标题由独立 Git conventions 按对应事件校验，避免误检 GitHub 自动生成的合并提交。

上次提交 `dade2ce` 的远端 Git conventions 通过，Local testing 在控制面测试清理
阶段失败：API Server 超过 envtest 默认 20 秒退出等待期限。修复将默认等待改为
有界的 90 秒，保留错误，不跳过测试。具体本轮本地和远端结果以交付消息及
GitHub Actions 对应提交为准，不能把旧的本地通过记录当作新提交的远端通过。

macOS 的协议/控制面验证不能替代真实沙箱、生产权限或隔离验收；Kind、
Linux/KVM 和上游部署测试分别记录，环境缺失不记为通过。

## 下一步优先级

1. 完成本轮本地统一 CI 与对应提交的远端 CI，确保 `dev` 是可重复验证的基线。
2. 在 x86_64 Linux 测试机部署 AgentENV v0.2.3，验证创建/发现/执行/stdin/取消/文件/删除与泄漏；通过后才开放 Registry。
3. 在同一测试机验收 Obscura native 的真实 Firecracker、vsock、CLEAN reset、网络与磁盘回收。
4. 复用共享 HTTP transport，补 OpenSandbox、CubeSandbox 专用 codec 和最小 hook，逐个执行固定版本准入。
5. 在需要时补 Gateway 端口代理；完成 Runtime 准入后再做 containerd/runsc、预热池和对比基准。

详细约束见 [测试指南](testing.md)、[Runtime 接入契约](plans/2026-10-16-runtime-contract-v1.md)
与 [自研 Runtime 说明](../runtime/README.md)。
