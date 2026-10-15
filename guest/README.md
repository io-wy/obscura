# Guest Runtime

Guest runner 是一个 TypeScript 包，负责 vsock 消息循环和固定版本 pi-agent 的适配边界。
Runner 协议定义在 `api/proto/agentruntime/v1/guest.proto`；`src/protocol.ts`
只处理传输帧和本地 TypeScript 类型。

Guest 包不得依赖 Go 或 Rust 的实现细节。Agent 输出以有大小限制的 Protobuf
事件载荷发送，不写入控制面元数据存储。
