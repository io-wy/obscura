# Rust Runtime

Rust Runtime 是唯一拥有宿主机操作权限的组件。其 orchestrator 将沙箱状态持久化到
SQLite，分配独立 TAP 网络，制作私有可写 rootfs 副本，并通过 UDS HTTP API 启动
Firecracker。`node` 二进制提供 gRPC/UDS 服务，以及 Axum 的 `/healthz` 和
`/metrics` 端点。

`vmm` 封装 Firecracker 专有调用；`storage`、`image`、`snapshot`、
`overlaybd`、`p2p` 和 `pool` 是独立模块。快照、OverlayBD、P2P 和预热池
**尚未接入生命周期**；guest vsock 执行路径已接入。稳定的调用方 ID 和请求指纹
支持 Controller 重试；重启隔离机制避免把失去归属的 VM 报告为 Ready。跨进程自动
恢复、生产镜像分发和 E2B envd 兼容仍未完成。Linux 要求见根目录 README，
接入约束见 [Controller 契约](CONTROLLER_CONTRACT.md)。需要 Rust 1.89 或更新版本。

当前自研 RL runtime 的第一批实现已接入现有 Firecracker 生命周期；containerd/runc
吞吐后端、runsc、预热池调度和性能基准仍属于下一阶段。此改动沿用当前工作区的
controller/runtime 协议，不扩展 Proto、CRD 或 Controller。

`ResetSandbox(CLEAN)` 先持久化 reset intent，再取消旧执行流、停止并回收 VM，
删除 TAP/nft 规则和私有磁盘，按 guest IP 清除两个方向的宿主机 conntrack，
从校验过的 rootfs/kernel 副本启动新 VM。guest 就绪后原子提交新的 episode、
generation、Ready 和 request receipt。同一个 reset request ID 重试不重复重建。
失败保留非 Ready 身份与资源归属，重启后的旧实例隔离，需人工证明归属后处理；
不会自动把旧 VM 接回池。Delete 在释放网络槽位前也必须清除 conntrack。

Linux 宿主机新增依赖 `conntrack-tools`（`conntrack` 命令、CAP_NET_ADMIN），
并保留 `ip`、`nft`、IPv4 forwarding、Firecracker 与 `/dev/kvm` 要求。
清理失败，包括工具缺失或权限不足，阻止 reset 重建或 IP 释放。命令构造已通过
fake runner 测试；实际 Linux 网络/KVM 隔离仍须部署环境验证。

环境 revision 绑定实际 rootfs/kernel SHA-256、runtime 版本、资源、启动参数和
已实施的网络/临时工作区策略；独立于 sandbox ID 和请求追踪 ID。可选 image pin
与实际 launch profile 比对，复制后的磁盘与 kernel 再校验。兼容旧 template alias，
因此它不是完整的生产 environment manifest admission。`environment` 模块提供完整
revision 的规范编码，但依赖锁、dataset、scorer pin 尚无实际内容验证器；RPC 对这些
请求返回 Unimplemented，持久工作区、凭证代理和最大生命周期也一样拒绝。
域名 allowlist 不会降级为全量放行；CHECKPOINT/TEMPLATE reset 尚不支持。

node 默认在 `OBSCURA_STATE_DIR/trajectory.spool` 启用本地轨迹元数据日志：单条
16 KiB、总量 256 MiB、权限 0600、独占 writer、header/body 独立校验和与未完成尾部
恢复。日志写满返回 ResourceExhausted，无自动丢弃/轮转。执行开始在发送 Run 前记录，
guest 事件和终态在转发前同步落盘；同 episode 内的 execution ID 不可重用。
创建/reset 记录生命周期；Delete 即便日志写满仍执行资源清理，以 SQLite tombstone
为准。记录失败不会继续转发，已启动的 episode 标记 Failed 并保留清理句柄。

这是一份审计元数据日志，原始 command/stdin/stdout/stderr 只存长度与 SHA-256，
不记录 env 值或原始错误。它不能回放任务内容，也不能直接代替 RL 训练数据集。
调用者必须保证 execution ID 等标识不含秘密；摘要不保证匿名化。
远端导出、内容存储、日志轮转、资源采样和策略事件自动采集尚未接入。
现有指标仅为 create/failure/delete/reset 操作计数器，reset 计数包含成功重试。

`scoring` 提供宿主侧 artifact manifest、opaque worker/ticket 和 provenance 校验，
recorded API 先写 handoff/result 摘要再返回或消费 ticket。它没有启动独立 scorer
服务。调用方仍须认证 worker、校验 CAS 的实际内容、施加独立隔离并持久化完整结果。
terminal evidence 必须引用 `HostExecution` scope 的 `Terminal`，不能引用 guest
输出序列。ticket 保存在内存，重启失效，跨重启去重仍由调用方实现。

执行 admission 和 guest 连接共用绝对 deadline；同步磁盘 I/O 故障不具备硬时间
上限。reset/delete 通过每 sandbox fence 排除命令启动，旧流取消并关闭旧 generation
socket。协议没有 external epoch，无法区分 reset 以后才到达的全新请求是否属于
旧调用方；上游必须对 reset 与新执行排序。外部 generation 字段属于未来独立协议讨论。

SQLite schema 已升级为 v2（episode/launch/reset/receipt 表）；不支持旧二进制降级。
完整交付进度与 Linux 验证门槛见 [实现计划](../docs/plans/2026-10-16-native-rl-runtime-plan.md)。
