# Kubernetes Controller 实施计划

> Agent 开发者：按 superpowers 工作流执行独立任务，先写有意义的失败测试，
> 最终验证前同时评审契约符合性和实现质量。

目标：通过 Kubernetes 期望状态和可插拔 Runtime 管理沙箱。
架构：Go Controller 和 Gateway 共享与提供方无关的适配器契约；Rust 负责宿主机
操作；第三方 API 适配器保留提供方自身的调度能力。
技术栈：Go、controller-runtime/client-go、Kubernetes CRD、Rust、tonic/Protobuf。

1. 定义类型化 CRD，并生成 schema、RBAC 和示例。
2. 测试并实现 Controller 生命周期、持久化绑定、不确定结果处理、到期、
   不可变启动规格和 finalizer 清理。
3. 测试并实现 Runtime 契约、Obscura gRPC 与 E2B HTTP 适配器。
4. 测试并实现 Rust 稳定身份、指纹存储和重启隔离。
5. 添加 Kubernetes Gateway 与可执行入口，并记录部署拓扑。
6. 运行组件测试、schema/API 准入验证和构建；评审最终改动，修复发现的问题，
   然后重新运行要求的验证。
