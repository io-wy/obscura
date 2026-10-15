# Obscura Kubernetes Controller 与 Runtime 设计

已确定采用 Kubernetes 控制面和 Runtime 适配器。现有 Rust Firecracker 服务是
Obscura 自研 Runtime；AgentENV 和 CubeSandbox 是在集群内部署的可选后端。
沙箱不一定是 Pod；各后端自己的调度器仍负责 VM 放置和容量管理。

## 第一阶段实现

- 在 `sandbox.obscura.dev/v1alpha1` 中定义命名空间级的 `Sandbox` 和
  `SandboxRuntime` 资源。
- 使用 Go controller-runtime manager，提供 leader election、Conditions、
  持久化后端绑定、过期处理和基于 finalizer 的清理。
- 生命周期适配器契约不依赖 Kubernetes 或生成的 Protobuf 类型。
- Obscura gRPC 适配器使用现有本地 Unix socket；面向 AgentENV/CubeSandbox
  的 E2B 风格 HTTP 适配器访问其集群内部服务。
- Kubernetes Gateway 写入期望状态，并通过会话适配器传送执行流，与 Controller
  分开部署。
- Rust 创建使用稳定 ID 和持久化请求指纹，并在重启后采取保守隔离措施。
  未验证仍存活的 VM 不会仅因进程句柄丢失就被报告为 Ready 或被销毁。

## 保证与边界

外部副作用发生前，先持久化 finalizer 和创建意图。Obscura 使用 Sandbox UID
作为稳定 Runtime ID，并拒绝以既有 ID 创建不同启动规格。未证明创建幂等的 HTTP
后端必须通过关联标记恢复，遇到不确定结果时拒绝自动重建。Conditions 区分连接
未知与实例缺失或失败。Gateway stdout 和凭据不写入 Sandbox status。每个沙箱
固定其 Runtime 配置和绑定，避免修改 runtimeRef 或端点后遗留旧实例。不支持的
资源与网络要求必须被拒绝。

原生传输首期为本地 UDS。中心 Controller 无法直接访问任意节点的 UDS：适配器
必须与 Runtime 同节点部署，或未来提供经过认证的节点传输。HTTP 后端通过集群
Service 接入。完整 envd/E2B SDK 兼容、Pod 后端、节点调度、存活 Firecracker
进程自动接管、快照可移植性和预热池属于后续里程碑。

## 验证

测试覆盖重复调谐、Controller 重启、创建后 status 写入失败、HTTP 创建结果不确定、
到期、删除重试、未知后端状态、配置漂移和会话授权。Rust 测试覆盖稳定 ID、指纹
冲突和重启隔离。验证生成的 CRD，并在二进制资产可用时用 envtest 验证 API Server
准入和 finalizer。真实 KVM 与后端 smoke 需要 Linux 部署，在当前 macOS 开发机上
明确尚未验证。
