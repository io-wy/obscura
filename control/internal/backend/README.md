# Runtime 后端适配器

`Registry.Open` 选择 `native`（别名 `obscura`），或为 `e2b`、`agentenv`、
`cubesandbox` 选择 HTTP 生命周期子集。Secret 凭据从 Sandbox 所在命名空间解析，
只保留在客户端，不写入 Observation。默认 Secret 数据字段为 `api-key`，也可显式指定。

## 原生 Rust Runtime

端点必须是绝对 Unix socket 路径或 `unix:///absolute/path`。CPU、内存、磁盘、
超时和是否允许出站流量会映射到现有 Protobuf API。Runtime 尚不强制执行主机名
白名单，因此此类请求会被拒绝。每次 Ensure 都会检查 Health 是否声明
`idempotent-create-v1` 且协议版本为 `agentruntime.v1`，然后把 Sandbox UID 作为稳定的 `sandbox_id`，连同
Controller 计算的不可变 `spec_hash` 一起发送。

Delete 只有在 Get 返回 Deleted 或 NotFound 后才算完成。Exec 以 Protobuf
事件流返回结果，使用显式截止时间或最长五分钟的默认值。执行流必须以
`result` 或 `error` 事件结束；提前 EOF 会作为中断上报，不会重放命令。

## 错误与重试

适配器返回统一的可用 `errors.Is` 判断的类别。Controller 只用类别决定状态与
后续动作，绝不从后端错误文本推断是否能重新创建。

| 类别 | 来源示例 | Controller 动作 |
| --- | --- | --- |
| `ErrUnavailable` | gRPC 断连/超时、HTTP 5xx、API Server 读取 Secret 失败 | 保留绑定，标记 Unknown，定时观察 |
| `ErrRateLimited` | HTTP 429 | 标记 RateLimited，定时观察 |
| `ErrAuth` | HTTP 401/403、Secret 缺失或无权限 | 标记 CredentialsUnavailable，等待凭据修复 |
| `ErrCapacity` | 原生节点 `ResourceExhausted` | 标记 CapacityUnavailable，不换节点重新创建 |
| `ErrInvalid` / `ErrUnsupported` / `ErrConflict` | 请求不合法、能力缺失、稳定身份冲突 | 报告确定性失败，保留身份和创建记录 |
| `ErrPrecondition` | 原生节点已隔离、身份 Creating/Failed/Deleted | 报告需要人工处理；删除时保留 finalizer |
| `ErrAmbiguous` | HTTP POST 结果不确定或发现不到既有实例 | 只继续发现，禁止再次 POST |

Kubernetes API Server 的 watch、连接恢复和 reconcile 出错后的限速重排由
controller-runtime/client-go 管理。写入创建意图或状态失败时返回错误，由队列重试，
不能先发创建请求。后端观察使用 15 秒请求上限和定时重排；HTTP client 与 gRPC
client 的底层连接可以重建，但重建连接并不授权重放创建或执行。原生创建只有稳定
UUID、规格指纹、同一节点数据库和 `idempotent-create-v1` 同时成立时才能重试。
HTTP 创建最多发送一次 POST；DELETE 可以重试，但 finalizer 只在资源缺失得到
确认后移除。执行流断开后保留已收到事件，不自动执行第二次。

当前定时重排没有按 `Retry-After` 调整，也没有按错误类别施加独立的指数退避。
大规模部署前需要结合上游限流配额和最大并发数确定退避/抖动策略。HTTP 上游
协议的 4xx/5xx 副作用保证、AgentENV/CubeSandbox 真实版本兼容性也需要在接入
指定版本时验证；在此之前，POST 错误一律按结果不确定处理。

## E2B 风格 HTTP 生命周期子集

适配器要求部署提供以下契约：

- `GET /v2/sandboxes`：返回 JSON 数组，支持元数据过滤、
  `state=running,paused`，并使用 `X-Next-Token` / `nextToken` 分页。
- `POST /sandboxes`：接收 templateID、timeout 和 metadata；成功响应包含
  sandboxID。这里使用许多 Gateway 支持的旧版创建端点。
- `GET /sandboxes/{id}`：返回 sandboxID 及 running/paused 状态。
- `DELETE /sandboxes/{id}`：成功请求后，再通过 Get 确认实例已经消失。

列表、过滤和分页语义于 2026-10-15 对照
https://github.com/e2b-dev/E2B/blob/main/spec/openapi.yml 核查。AgentENV 和
CubeSandbox 的实际部署必须先实现这一准确的版本化生命周期子集，才能被选为后端。
类型名称不表示它们每个发布版本都已支持。发现失败时不会回退到只列出 running
实例的旧接口，因为那会漏掉 paused 实例并造成重复创建。

发现过程按 `obscura_uid` 搜索，核对返回的 `spec_hash`，并拒绝多个匹配实例。
HTTP POST 不被假定为幂等；只有 Controller 的 `Request.AllowCreate` 授权唯一
一次创建尝试时才允许创建。POST 超时或返回结果不确定后，重试必须设置
`AllowCreate=false`：发现既有实例，或返回 `ErrAmbiguous` 交由运维处理。
不能仅因为发现结果为空就再次 POST。

在适配器能够验证资源覆盖和网络限制确实生效前，这些要求会被拒绝；资源应通过
后端模板选择。目前只接受不限制出站流量。envd 访问令牌会被丢弃；不宣称完整
E2B SDK 支持，HTTP Exec 返回 `ErrUnsupported`。请求有超时上限，重定向会被拒绝，
以保护凭据。
