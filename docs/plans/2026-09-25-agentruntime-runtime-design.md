# AgentRuntime 沙箱 Runtime 设计

## 目标

构建单节点 AgentRuntime 沙箱原型：以 Firecracker 为隔离边界，Rust 为特权数据面，
Go 为控制面，TypeScript 为 guest 内的 Agent Runtime。首个里程碑应在具备 KVM 的
Linux 宿主机上端到端运行一个真实 Agent 任务，并在成功、超时和失败时清理所有
宿主机资源。

## v0 暂不包含

- 多节点调度或分布式控制面。
- 通用容器或 VM 管理 API。
- 持久化用户卷、共享文件系统或跨沙箱网络。
- 生产级镜像分发和远程快照存储。
- 向客户端直接暴露 Firecracker API。

## 架构

```text
客户端 / 测试程序
        |
        v
Go 控制面
  API、状态机、配额、幂等、元数据
        | 带版本的 Unix socket 协议
        v
Rust Runtime
  进程生命周期、Firecracker API、磁盘、TAP、vsock、清理
        | KVM
        v
Firecracker microVM
  guest init -> agent runner -> TypeScript Agent 适配器
```

控制服务负责期望状态和对外身份。Runtime 负责全部宿主机能力，也是唯一允许创建或
销毁 Firecracker 进程、TAP 设备、VM 磁盘和沙箱目录的组件。客户端不会得到
Firecracker socket 或宿主机路径。

### 控制面

Go 服务暴露精简的版本化 API：

- `CreateSandbox(template_ref, resources, network_policy)`
- `GetSandbox(id)`
- `Execute(id, command, input, deadline)`，以事件流返回结果
- `DeleteSandbox(id)`
- `Health()`

沙箱记录包含不可变的 `sandbox_id`、模板/镜像引用、资源限制、生命周期状态、
时间戳和已脱敏的失败原因。状态转换应在副作用之前或与副作用一起持久化：

`Creating -> Ready -> Running -> Paused -> Deleted`

`Failed` 是当前尝试的终态，并提供可安全重试的清理路径。创建和删除请求按请求键
保证幂等。v0 可以使用本地 SQLite 或文件存储，但仓储接口不能把该选择暴露给
API 层。

### 数据面

Rust Runtime 通过 Unix domain socket 向 Go 服务提供精简 gRPC 接口，契约位于
`api/proto/agentruntime/v1/runtime.proto`。每个请求带有 request ID、sandbox ID、
截止时间和操作名称。数据面只把显式句柄与资源路径作为不透明标识返回。

职责如下：

1. 分配私有状态目录并验证镜像清单。
2. 使用最小机器配置启动 Firecracker。
3. 配置一个根磁盘、可选可写层、一个 vsock 通道及控制面指定的网络策略。
4. 等待 guest 就绪并传送 Agent 事件流。
5. 按停止、强制终止、回收的顺序处理进程；即使启动中途失败，也清理 TAP、
   socket 和临时文件。

Firecracker 进程使用专用 Unix API socket 和最小权限启动。实现必须通过白名单
配置构造器生成配置，不能直接转发控制面提供的任意 JSON。

### Guest Agent Runtime

Guest 包含精简的 init/runner 进程和 TypeScript 适配器。Runner 通过 vsock 使用
四字节大端长度前缀传输 Protobuf 消息。帧格式见
`shared/protocol/v1/frame-format.md`：

- 宿主机到 guest：run、cancel、ping 消息。
- guest 到宿主机：ready、started、stdout、stderr、result、error 事件。

每个事件包含 `sandbox_id`、`execution_id`、单调递增序号和时间戳。载荷有大小
上限并支持背压。pi-agent 适配器把该协议映射到固定版本的 pi-agent；其专有对象
不跨越 Go/Rust 边界。

## 存储、镜像与分发

v0 使用三层：

1. 按摘要标识的不可变基础镜像，包含 guest 内核、根文件系统、Node.js 和 runner。
2. 从基础镜像创建、每个沙箱独享的可写磁盘。
3. 可选的执行临时目录，具有明确的大小限制。

镜像清单记录架构、内核/rootfs 摘要、runner 协议版本、兼容的 Firecracker 版本
范围和创建元数据。最初按本地路径分发，但清单与摘要接口应允许日后接入对象存储
解析器。启动前验证镜像；数据面不接受可变 tag。

## 网络

v0 支持默认拒绝的出站策略和显式白名单。数据面为每个沙箱建立独立 TAP 接口，
分配唯一 guest MAC 与地址，并随 VM 一起清理。宿主机网络被封装在 provider
接口后，便于未来替换为无 root 代理或租户网关。不暴露从宿主机到 guest 的入站
访问；控制流量使用 vsock。

## 可观测性与安全

- 结构化日志使用 `sandbox_id`、`execution_id`、`request_id` 作为关联字段。
  v0 暂不接入 OpenTelemetry。
- Prometheus 指标覆盖生命周期计数、启动与就绪耗时、执行延迟、资源清理失败和
  镜像缓存命中率。
- Guest stdout/stderr 作为事件流发送，并单独限速。
- Secret 通过短时通道在执行时注入，不持久化到沙箱元数据或普通日志。
- 接收沙箱前，宿主机必须验证 KVM 可用性、Firecracker 二进制摘要、镜像清单、
  文件系统权限和资源上限。
- 尽可能让 runner 和 Agent 在 guest 内以非 root 身份运行；宿主机权限留在
  Rust 数据面。

## 快照、克隆与预热池路线

快照不属于首个纵向闭环。后续契约如下：

- `CreateSnapshot(sandbox_id, quiesce=true)` 返回不可变快照 ID。
- `CloneSnapshot(snapshot_id)` 创建新的沙箱身份、MAC、网络身份、执行凭据和
  可写磁盘层。
- 快照元数据记录 Firecracker 兼容性、guest 架构、内核/rootfs 摘要、协议版本
  和源镜像摘要。
- 快照池持有预热容量并向调度器出租快照；同一可变快照绝不能交给两个沙箱。

启用池之前，测试必须证明克隆沙箱不会共享可写状态、凭据、网络身份或执行序号。

## 测试策略

仓库把测试分为三层：

1. 状态转换、协议编码、镜像清单、幂等、清理计划和策略校验的纯单元测试。
2. 使用模拟 Firecracker 和网络 provider 的宿主机集成测试，可在 macOS 和普通
   CI runner 上运行。
3. 在可信 Linux runner 上运行 KVM smoke：启动真实 guest，执行 TypeScript
   Agent 命令，传送输出，并验证超时后的清理。

初始成功标准是一个稳定的 KVM smoke 测试，以及通过模拟实现覆盖失败清理。
正确性确认后再测量冷启动、就绪、克隆延迟，以及控制面和数据面的峰值 RSS。
