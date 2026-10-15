# obscura

一款为 AgentRuntime 提供的“暗箱”。

协作规范、分支 / commit / tag 约定及命令用法见 [CONTRIBUTING.md](CONTRIBUTING.md)。
`make git-help` 查看终端命令；`.claude/commands/` 提供 `/branch`、`/commit-push`、
`/pr` 和 `/release-tag`。

## macOS 本地测试

`make dev-bootstrap` 准备依赖，`make test-mac` 运行单元和真实 Kubernetes API 下的
控制面联调。`make dev-up` 启动独立 API Server / etcd、Controller、Gateway 与
gRPC 模拟 Runtime，供 curl / kubectl 交互；另一个终端执行 `make dev-smoke` 自动走通
生命周期。无需 Docker 或 KVM；模拟器不执行宿主机命令。真实 VM、vsock 和网络隔离
仍需 Linux/KVM，完整能力矩阵与步骤见 [测试指南](docs/testing.md)。
Docker 可用时，`make kind-up && make kind-smoke` 额外验证镜像、Pod、Service 和
RBAC；`docker compose run --build --rm control-tests` 在 Linux 容器中运行控制面联调。
这两层的用途及清理命令也在测试指南中。

## Kubernetes 控制面

新增 `Sandbox` / `SandboxRuntime` CRD、controller-runtime 控制器和独立 Gateway。
现有 Rust Firecracker 服务就是 Obscura 的自研 Runtime，通过后端适配层与
AgentENV / CubeSandbox 等实现并列接入。创建、状态观察、有效期和删除由控制器
管理；命令执行流经 Gateway 直达 Runtime。

- `control/cmd/controller`：CR 生命周期、持久化绑定、finalizer 和超时回收。
- `control/cmd/gateway`：创建 CR 的异步 API、自研 Runtime 执行流。
- `control/internal/backend`：自研 gRPC 和 E2B 风格 HTTP 生命周期适配器。
- `deploy/`：CRD、命名空间 RBAC、Deployment、Service 和示例。

这是第一阶段实现：自研 Runtime 为单节点 UDS 接入；HTTP 适配器要求后端提供
E2B v2 元数据查询能力，尚未经真实 AgentENV / CubeSandbox 部署验收。HTTP 后端
执行、文件和 PTY 尚未接入，Gateway 的 `/v1` API 不宣称完整 E2B SDK 兼容。
详见 [Kubernetes 使用说明](docs/kubernetes.md) 和
[Runtime 接入契约](runtime/CONTROLLER_CONTRACT.md)。

## Runtime 原型

第一阶段采用以下固定边界：

- Rust 数据面直接管理 Firecracker
- Go 控制面通过 gRPC + Protobuf over Unix Domain Socket 调用数据面
- guest 内 TypeScript runner 通过 vsock + Protobuf framed messages 通信
- OCI/Dockerfile 构建 guest rootfs，再转换为 Firecracker ext4 镜像
- 本机文件系统 + SQLite 元数据
- TAP + NAT + nftables 默认拒绝
- 结构化日志 + Prometheus metrics，不绑定 tracing 平台

代码目录按职责隔离：`api/` 放跨语言协议，`control/` 放 Go 控制面，
`runtime/` 放 Rust 主机能力，`guest/` 放 guest runtime，`shared/` 放协议
说明与测试样例。详见 [Runtime 设计](docs/plans/2026-09-25-agentruntime-runtime-design.md)
和 [v0 实施计划](docs/plans/2026-09-25-agentruntime-v0-implementation-plan.md)。

当前已接入 Rust 节点上的 SQLite、TAP/nftables、Firecracker 启动及 guest vsock
执行路径；Go 控制面通过 Kubernetes CRD 管理生命周期，并提供 gRPC/UDS 与 E2B
风格 HTTP 后端适配器。
guest OCI 镜像可构建，但真实 KVM 启动与端到端执行尚未经 Linux 宿主机验证。
E2B envd 完整兼容、镜像分发、快照/克隆、OverlayBD/P2P 和预热池尚未完成，
不能把当前原型当成完整 E2B SDK。

## Linux 节点试运行

需要 Linux/KVM、Docker、`iproute2`、`nftables`、`mkfs.ext4`、可引导且架构匹配
的 Firecracker 内核和 Firecracker 二进制。执行前确认宿主机出口网卡及防火墙策略；
`allow_egress` 默认为 false，已有宿主防火墙规则仍可能影响通信。

```sh
sudo mkdir -p /var/lib/obscura /run/obscura
sudo sysctl -w net.ipv4.ip_forward=1
sudo ./scripts/build-guest-image.sh /var/lib/obscura/guest.ext4
export OBSCURA_STATE_DIR=/var/lib/obscura/state
export OBSCURA_NODE_SOCKET=/run/obscura/node.sock
export OBSCURA_KERNEL=/var/lib/obscura/vmlinux
export OBSCURA_ROOTFS=/var/lib/obscura/guest.ext4
export OBSCURA_FIRECRACKER=/usr/local/bin/firecracker
export OBSCURA_UPLINK=eth0
export OBSCURA_TEMPLATE_REF=guest:local
sudo -E cargo run --manifest-path runtime/Cargo.toml --bin node
```

另一个终端按 [Kubernetes 使用说明](docs/kubernetes.md) 启动 controller 和 Gateway，
并创建指向 `/run/obscura/node.sock` 的 `SandboxRuntime`。
启动前需创建节点 socket 的父目录并保证权限；`OBSCURA_BOOT_ARGS` 可覆盖默认的
`root=/dev/vda rw init=/sbin/init` 内核参数。节点重启后会把 SQLite 中原有活跃
实例标为 Failed 并隔离，保留磁盘和网络绑定；禁止自动重建或删除无法确认归属
的 VM。节点可以服务新实例。须先隔离旧节点进程，确认并清理相应 Firecracker、
TAP、nft 表和沙箱目录，再审计元数据。**不要在进程或网络资源仍存在时解除
隔离。**目前尚无跨进程自动恢复机制，详见上面的接入契约。

## AgentRuntime 沙箱模板

`templates/agentruntime-sandbox/` 是从 TencentCloud/CubeSandbox 与
kvcache-ai/AgentENV 提炼出的通用开发模板。它不绑定 Rust、Go 或某一个
沙箱后端，提供可复制的工程治理骨架：

- GitHub Actions、CODEOWNERS、Issue/PR 模板和安全默认值
- `.claude` 的开发 skill、reviewer agent 与 PR review command
- `.agent` 的运行时契约、沙箱生命周期和 CI 故障排查 skill
- KVM/权限检查、集成测试入口、失败清理和发布前检查

进入模板目录阅读 [README](templates/agentruntime-sandbox/README.md)，再按
项目语言和 runtime 后端替换其中的命令占位符。
