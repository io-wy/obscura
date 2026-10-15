# AgentRuntime 沙箱 v0 实施计划

## 里程碑 1：仓库骨架

1. 建立 `control/` Go 模块，包含 API 类型、生命周期状态机和存储接口。
2. 建立 `runtime/` Rust workspace，包含 RPC 类型、资源 provider trait，
   以及不依赖宿主机能力的空实现或模拟后端。
3. 建立 `guest/runner/` TypeScript 包，包含 Protobuf 帧传输、事件类型和固定
   版本 pi-agent 的适配边界。
4. 添加共享协议测试样例目录和兼容版本。

## 里程碑 2：模拟实现的纵向闭环

1. 先写失败测试，覆盖创建、执行、删除、幂等重试，以及每个故障注入点后的清理。
2. 基于模拟 Runtime RPC 实现 Go 控制服务。
3. 基于模拟 Firecracker、磁盘、网络和 guest provider 实现 Rust 生命周期状态机。
4. 实现 TypeScript runner 的启动、执行、取消行为和帧协议黄金测试。
5. 添加覆盖完整模拟流程的本地 CLI 或测试程序。

## 里程碑 3：真实 Linux/KVM 路径

1. 构建包含内核、根文件系统、Node.js、runner 和健康端点的最小 guest 镜像。
2. 用带配置白名单的 Rust Firecracker 客户端替换模拟调用。
3. 接入根磁盘、TAP 接口、vsock、资源限制和就绪检查。
4. 明确清理顺序，并在测试后审计宿主机资源。
5. 在可信 Linux KVM CI runner 上运行真实 smoke。

## 里程碑 4：运行基础

1. 添加结构化日志、指标、脱敏和请求/追踪关联。
2. 添加镜像清单、摘要验证、本地缓存和分发接口。
3. 添加执行截止时间、输出限制、出站策略验证和容量检查。
4. 记录 macOS 开发与 Linux/KVM 集成测试流程。

## 里程碑 5：快照与调度

1. 添加静止/恢复 hook 和不可变快照元数据。
2. 实现克隆身份重新生成和可写层隔离测试。
3. 添加带资源核算的单节点放置策略。
4. 添加具有租约、预热、排空和修复状态的快照池。
5. 最后才把控制面扩展到多节点。

## v0 完成标准

- Linux KVM runner 从按摘要固定的镜像创建沙箱。
- TypeScript Agent 任务在 guest 内执行并返回事件流。
- Go API 不依赖 Firecracker 专有 JSON 和宿主机路径。
- 超时、guest 崩溃、Firecracker 启动失败和控制面重试都有稳定测试。
- 删除沙箱后不残留 Firecracker 进程、TAP 设备、socket 或临时状态目录。
