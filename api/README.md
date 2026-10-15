# API 契约

`api/proto/agentruntime/v1/` 是 Runtime 与 guest Protobuf 消息的唯一协议源。
使用 Buf 配置或 `scripts/generate-go-proto.sh` 生成语言绑定，输出到被忽略的
`control/gen/` 目录。Kubernetes JSON API 类型单独放在 `control/api/v1alpha1`；
`make manifests` 重新生成 deepcopy 代码和部署用 CRD。

- `runtime.proto`：Go 控制服务通过 gRPC/UDS 调用 Rust Runtime。
- `guest.proto`：Rust Runtime 通过 vsock 帧协议与 TypeScript guest runner 通信。

独立 Go 服务负责放置策略和对外路由。Kubernetes Controller 负责期望状态和持久化的
后端绑定；Rust 负责实际 VM 与存储状态。稳定的调用方 ID 和规格指纹支持 Controller
重试创建。不要让生成的传输类型进入 VMM 内部。修改传输字段时，先更新 proto，
再重新生成语言绑定。
