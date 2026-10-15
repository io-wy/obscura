# AgentRuntime 沙箱开发指南

本仓库包含短时 Agent 沙箱的控制面和隔离 Runtime。后端可以是 microVM、容器或
其他隔离边界，但运维契约相同。

## 工作规则

1. 编码前阅读受影响模块、测试和生命周期文档。
2. 明确宿主机权限、持久化和网络策略中的不确定性。
3. 优先采用满足请求的最小改动。
4. 修改行为时先添加回归测试。
5. 将 Runtime 状态与依赖缓存、源文件分开。
6. 除非某个范围明确的宿主机准备步骤需要提权，否则以非 root 用户运行。
7. 测试中绝不使用生产凭据或在线沙箱服务。

## 生命周期不变量

- 沙箱身份唯一，删除后不得复用。
- 捕获或派生失败时，恢复源实例原先的稳定状态，或明确销毁；不得留下只发布一半
  的模板。
- 已发布模板和快照引用持久化产物身份，不引用节点本地路径。
- 只读卷可以共享；可写卷需要明确归属策略，子实例分配失败时必须回滚。
- 关闭和清理必须幂等。

## 验证

```bash
make template-check
make fmt
make test
make test-integration
```

`make test-integration` 预计需要 Linux 能力或 `/dev/kvm`。除非 runner 明确可信且
隔离，否则在 CI 中保持手动触发。

## GitHub 与发布策略

- 使用 Conventional Commit 前缀（`feat:`、`fix:`、`ci:`、`docs:`、`chore:`）。
- 让 `CODEOWNERS` 与 Runtime、存储、网络和 CI 的实际归属保持一致。
- 发布 tag 使用 `vMAJOR.MINOR.PATCH`；tag 必须与 Runtime 构建产物的版本一致。
- 安全漏洞通过私有安全报告渠道提交，不使用公开 Issue。
