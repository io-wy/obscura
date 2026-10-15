# Go 控制面

Kubernetes 控制面由 `cmd/controller`（controller-runtime）和 `cmd/gateway`
（期望状态 HTTP API 与执行流）组成。类型化 CRD 位于 `api/v1alpha1`，与提供方
无关的适配器位于 `internal/backend`。部署与 API 用法见
[Kubernetes 指南](../docs/kubernetes.md)。

Controller 通过 controller-runtime manager 的缓存、watch 和 `client.Client`
调谐资源；controller-runtime 底层使用 client-go，本模块也显式依赖 client-go。
控制器没有另写一套原生 clientset/informer 循环。

`internal/controller` 调谐 Sandbox 资源、持久化 Runtime 绑定，并在释放 finalizer
前确认清理完成。`internal/kubegateway` 写入期望状态，把执行会话发送到已绑定的
Runtime。原生适配器通过本地 Unix socket 上的 gRPC 调用 Rust 节点；HTTP 适配器
为兼容的 Runtime 实现 E2B 风格的部分生命周期 API。沙箱、镜像、快照和磁盘状态
由 Runtime 管理。HTTP 适配器尚未实现 envd 或完整 E2B SDK 兼容。
