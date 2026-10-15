# HTTP 会话与多租户控制面设计

## 目标与边界

第一阶段同时补齐两块能力：为 E2B 风格 HTTP Runtime 适配器增加命令执行、
文件读写、PTY 会话和 HTTP 端口代理；把当前单 namespace、单 API key 的 Gateway
扩展为共享的多租户入口，并实现可在多个 Gateway 副本间正确工作的配额预留。

Gateway 继续只公开 Obscura 自己的 `/v1` API。HTTP Runtime 使用本文定义并测试的
窄上游契约，不承诺完整 E2B SDK、envd 或任意版本第三方 API 兼容。第一阶段不实现
出站流量、累计文件传输量或请求速率配额。

## Kubernetes Operator 边界

控制面保持 Kubernetes CRD + Operator 架构。`Sandbox`、`SandboxRuntime` 和新增的
`SandboxTenant` 是期望状态及持久化协调记录；controller-runtime reconciler 负责
Runtime 生命周期、状态观察、到期删除、孤儿预留回收和 finalizer 清理。Gateway 是
北向认证和会话数据面入口，不代替 Operator，也不直接完成异步 Runtime 生命周期。

`SandboxTenant` 是集群级资源，包含：

- 唯一租户 ID 和该租户专用的 Sandbox namespace。
- 位于 Gateway 管理 namespace 的一个或多个 API key Secret 引用。
- 最大 Sandbox 数、单实例 CPU/内存/磁盘、总 CPU/内存/磁盘、最长运行时间、
  最大并发执行和最大并发 PTY 会话。
- status 中按稳定 Sandbox ID 记录的资源预留，以及带过期时间的会话租约。

API key 值只存入 Secret。轮换时先增加新 Secret 引用，再移除旧引用；移除引用即
撤销凭据。Gateway watch Tenant 和凭据 Secret，使用常量时间比较把请求映射到租户。
客户端不能选择 namespace，所有 Sandbox 查找和操作只使用认证租户绑定的 namespace。

## 原子配额预留

Gateway 根据 `Idempotency-Key` 和租户 ID 生成稳定 Sandbox 名称。创建 Sandbox CR
之前，它读取最新的 `SandboxTenant`，检查单实例限制及当前 allocations 汇总，并用
带 `resourceVersion` 的 status patch 写入预留。冲突时重新读取、重新计算并重试，
因此两个 Gateway 副本不能基于同一个旧快照同时越过上限。

预留键是 Sandbox 名称。相同幂等请求复用既有预留，不重复占用；不同请求不能复用
同一键。Creating、Ready、失败待清理和 Terminating 均持续占用。Controller 只有在
后端确认资源不存在后才删除 allocation，然后移除 Sandbox finalizer。Gateway 在
预留成功但创建 Sandbox CR 失败时保留一条短期 pending allocation；Tenant reconciler
仅在宽限期结束且通过 APIReader 确认对应 Sandbox 不存在后回收它。

执行和 PTY 开始前以相同的乐观并发方式创建短期 session lease。正常结束、客户端
断连和取消时主动释放；Gateway 崩溃后由过期时间回收。文件请求和 HTTP 端口代理
首期不占会话并发配额，但仍受请求超时和传输大小限制。

## Backend 能力模型

现有生命周期 `Backend` 保持最小职责。会话能力拆成可选接口：`Executor`、
`FileSystem`、`PTY` 和 `PortProxy`。Gateway 对不支持的能力返回统一的 501 错误，
而不是要求每个后端提供空实现。原生 Runtime 继续提供现有执行能力；本阶段新增的
文件、PTY 和代理先由 HTTP Runtime 实现。

HTTP Runtime 的窄契约包括：

- 命令执行：向绑定 handle 的 commands 端点 POST JSON，接收 NDJSON 事件流。
- 文件读取和写入：以 URL query 传递规范化绝对路径，以二进制正文传输内容。
- PTY：Gateway 与 Runtime 各建立 WebSocket，双向转发有界帧，并传播 resize、
  input、output、exit 和 error 消息。
- 端口代理：校验端口后转发 HTTP method、相对路径、query、选定 headers 和流式
  body；不接受由客户端指定的任意目标主机。

确切路径、消息 schema、允许转发的 header 和限制值在实施计划中固定，并由模拟
HTTP Runtime 的契约测试锁定。

## 超时、取消、限制与错误

每次操作的 deadline 取客户端请求值、Gateway 操作上限和 Sandbox 剩余寿命的最小值。
HTTP request context 取消必须立即取消上游请求或关闭 WebSocket；命令和 PTY 不自动
重放。请求体、文件大小、单个流事件、累计命令输出和 WebSocket 帧均设置显式上限。
超过限制会取消上游并返回确定的限制错误。

Gateway 对尚未开始的响应使用统一 JSON 错误结构，稳定公开错误 code，不暴露后端
响应正文或凭据。认证失败返回 401；租户或 Sandbox 不可见统一表现为 404；配额为
429；非法请求为 400；状态冲突为 409；能力缺失为 501；上游认证、限流、超时和
不可用映射为稳定的 502/503/504。流开始后不能修改 HTTP 状态时，NDJSON 发送终止
错误事件，WebSocket 使用约定 close code 和不含敏感信息的 reason。

## 验证

测试遵循先失败后实现。模拟 HTTP Runtime 覆盖请求路径、认证 header、JSON/body、
NDJSON 分片、非终止 EOF、输出上限、WebSocket 双向流、端口代理的状态与流式正文、
客户端断连、context 取消、deadline 以及所有错误类别。

Gateway 和 envtest 覆盖多 API key 轮换/撤销、跨租户 ID 猜测、namespace 固定、
单实例与总量限制、两个 Gateway 并发预留、幂等重试、Creating/Terminating 占用、
后端确认清理后释放、pending allocation 回收及 session lease 崩溃回收。最终运行
Go 单元/竞态测试、CRD schema 测试、envtest、manifest 渲染和现有控制面 smoke；
真实第三方 Runtime 和 Linux/KVM 仍作为独立环境验收，不由模拟测试代替。
