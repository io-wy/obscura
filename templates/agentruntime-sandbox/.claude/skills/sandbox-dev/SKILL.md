---
name: sandbox-dev
description: 使用仓库 Make 目标和宿主机检查构建、测试并排查 AgentRuntime 沙箱。
---

# 沙箱开发

使用稳定入口，避免自行拼凑命令：

```bash
make template-check
make sandbox-check
make test
make test-integration
```

运行集成测试前，确认 runner 是 Linux、Runtime 用户非 root，且具备所需的
`/dev/kvm`、网络命名空间或设备权限。分开存放 `SANDBOX_STATE_DIR` 和
`SANDBOX_DEPS_DIR`。

测试失败时：

1. 保留第一条可采取行动的错误及 Runtime 版本。
2. 重试前运行 `make clean`，避免陈旧 VM 或容器进程掩盖真实故障。
3. 缩小到受影响的生命周期操作：创建、启动、暂停、恢复、捕获、派生或删除。
4. 改变行为前添加回归测试。

不要为来自不可信 fork 的 Pull Request 启用特权 CI 任务。
