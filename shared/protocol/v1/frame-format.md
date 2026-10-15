# Protobuf 帧格式

Rust 数据面与 guest runner 通过 Firecracker vsock 通信。传输内容是一系列带长度
前缀的 Protobuf 消息：

```text
┌──────────────┬──────────────────────────────┐
│ uint32 大端  │ Protobuf 消息字节              │
│ 载荷长度    │ payload_size 字节              │
└──────────────┴──────────────────────────────┘
```

v0 的载荷上限为 1 MiB。接收方必须拒绝零长度帧、超过配置上限的帧和截断的载荷。
解码器必须同时支持跨多次读取的拆分帧，以及单次读取中的多个帧。Protobuf 消息类型
和协议版本通过 guest runner 的就绪事件协商。帧编解码器只负责传输，不应了解
Firecracker 或 pi-agent 类型。
