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
