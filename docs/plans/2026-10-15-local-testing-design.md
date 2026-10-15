# macOS 本地测试体系

目标：在 macOS 原生运行真实 API Server / etcd、Controller manager、Gateway，
从 HTTP 请求验证 CRD → watch/cache → reconcile → native gRPC/UDS → NDJSON。
所有组件使用独立临时状态，不读取用户当前 kubeconfig，不要求 Docker 或 root。

选择 envtest 作为默认开发环境：比仅 fake client 测试覆盖更多 Kubernetes 语义，
比默认要求 Docker/Kind 更轻量。Kind 适合验证部署、RBAC、Service 等集群行为；
Linux/KVM 用于真实 Rust Runtime 与 guest、网络隔离验收。这三层不能互相替代。

本地 Runtime 是专用内存模拟器，复用 native RPC 与 Registry，不执行宿主机 shell。
支持稳定 ID/hash、幂等创建、查询、删除确认、固定执行事件、不可用与阻塞删除故障。
运行时状态通过仅监听 loopback 的开发端点查看和调整。

`make dev-up` 启动长驻环境，输出 Gateway、开发控制端点、临时 kubeconfig 和 API key。
Ctrl-C 停止 manager、HTTP、gRPC、API Server 和 etcd，删除临时状态。
`make dev-smoke` 验证已运行环境；`make test-controlplane` 自行启动和清理同一环境，
验证认证、幂等、执行流、删除 finalizer、到期回收、故障恢复与 controller 重启。
自动测试对缺少 envtest 资产明确失败，不假装成功或静默 skip。

普通 `make test` 保持无需集群的快速层；`make test-mac` 聚合快速层与控制面联调。
bootstrap / doctor 明确区分必需工具、可选 Docker 和 Linux/KVM 条件。
CI 使用 macOS 验证同一本地控制面路径；文档列出能力矩阵、操作步骤和证据边界。
追加 Docker/Kind 层：Compose 以 Linux 容器运行控制面测试；Kind 专用集群验证
生产镜像、RBAC、Deployment、Service，并单独运行 native 协议模拟 Runtime 镜像。

实现步骤：先补模拟器与联调的失败测试；实现模拟器和 envtest harness；
增加 CLI、smoke、bootstrap/doctor 与 Make targets；补测试文档和 CI；
运行单元、真实 envtest 联调和手工 CLI smoke，审查清理与错误路径。

当前工作区包含前序未提交实现，直接在当前工作区增量建设，避免 worktree 遗漏这些依赖。
本任务不提交或推送代码。
