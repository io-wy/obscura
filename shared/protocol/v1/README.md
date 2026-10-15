# AgentRuntime 协议 v1

Go 控制面、Rust 数据面和 TypeScript guest runner 共用协议标识 `agentruntime.v1`。

Go/Rust 控制通道使用 `runtime.proto` 中的消息和服务。Rust/guest 通道使用
`guest.proto` 中的消息，通过 Firecracker vsock 按 `frame-format.md` 定义的
长度前缀格式传输。每个事件都带有在单次执行中单调递增的 `sequence`。
实现必须拒绝未知消息类型、超大帧和不匹配的协议版本。

本目录的测试样例刻意保持精简和稳定；各语言实现应针对它们添加黄金测试。
