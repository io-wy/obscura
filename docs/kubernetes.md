# Kubernetes Controller + Runtime

macOS 可直接运行 `make dev-up` / `make test-controlplane`，使用独立的真实 API Server
与模拟 Runtime 联调 Controller/Gateway，无需 Docker/KVM。环境边界、故障注入和
完整命令见 [测试指南](testing.md)。下文自研 Runtime 部署指真实 Linux/KVM 后端。

Obscura 使用 `sandbox.obscura.dev/v1alpha1` 的三个资源。`Sandbox` 表达不可变的
启动规格、有效期和期望存在的实例；`SandboxRuntime` 配置某个后端的入口、模板
映射和 Secret 引用；集群级 `SandboxTenant` 将凭据关联到一个独占命名空间，
记录沙箱资源配额和执行/PTY 会话租约。沙箱规格不可修改；需要改变规格时创建新的 Sandbox。

控制器首次处理资源时，先写 finalizer，再持久化绑定和创建意图，最后调用后端。
绑定固定 Runtime 配置、模板、Sandbox UID 和规格摘要，所以修改或删除原来的
Runtime 配置不会把已有实例的操作重定向到其他后端。凭据值不写入 status。
有效期从 CR 创建时间开始计算，Pending 时间也计入有效期。

Gateway 按 `X-API-Key` 解析 Tenant，仅操作该 Tenant 的命名空间。凭据 Secret
位于 Controller 的 `--namespace` 指定的认证命名空间，由 Tenant reconciler
转换为 SHA-256 摘要；Gateway 不再读取进程环境变量 `OBSCURA_API_KEY`。
同一 Tenant 可配置多份凭据支持轮换，Secret 更新后经 reconcile 生效。
命名空间不可被多个 Tenant 同时声明；存在歧义时拒绝鉴权。CR status、Tenant、
绑定配置和 Runtime 对象由管理员维护，不能给普通租户随意写权限。
删除 Tenant 前先清理其全部沙箱，原后端 Secret 保留到清理结束。

## 自研 Runtime 本地开发

按照根 README 启动 Linux/KVM Rust 节点，在同一个 Linux 节点运行控制器和
Gateway，使用同一个 Kubernetes 配置和 UDS。Runtime 不需要 Kubernetes 凭据。
本版使用 Kubernetes 1.35 系列客户端和 envtest 目标；需要访问集群的权限。

```sh
make proto-generate manifests build-control
kubectl apply -k deploy/crds
kubectl wait --for=condition=Established crd/sandboxtenants.sandbox.obscura.dev --timeout=60s
kubectl apply -f deploy/base/namespace.yaml
kubectl -n obscura-system create secret generic obscura-gateway-key \
  --from-literal=api-key="$OBSCURA_API_KEY"
kubectl apply -f deploy/base/tenant.yaml
cd control
go run ./cmd/controller --namespace=obscura-system
```

另一个终端等待 Tenant 凭据就绪后启动 Gateway。客户端使用上述 Secret 对应的 key：

```sh
kubectl wait --for=condition=Ready sandboxtenant/obscura-default --timeout=120s
cd control
go run ./cmd/gateway --namespace=obscura-system --listen=127.0.0.1:9000
```

管理员可应用 Runtime 和示例沙箱，观察状态。此文件直接创建 Sandbox CR，
不会经过 Gateway 的配额预留；日常租户应使用后文的 Gateway API 创建沙箱：

```sh
kubectl apply -f deploy/examples/obscura.yaml
kubectl -n obscura-system get sandboxes -w
kubectl -n obscura-system describe sandbox demo
kubectl -n obscura-system delete sandbox demo
```

UDS 权限是 0600，控制器与 Gateway 需要可访问该 socket 的身份。当前不能在
一台机器上通过 UDS 操作另一台机器的 Rust 节点。

## 集群部署

在仓库根目录构建并发布控制面镜像；使用实际 registry/tag 替换清单中的
`obscura-control:dev`。镜像包含 controller 和 gateway 两个可执行文件。

```sh
docker build -f control/Dockerfile -t obscura-control:dev .
kubectl apply -f deploy/base/namespace.yaml
kubectl -n obscura-system create secret generic obscura-gateway-key \
  --from-literal=api-key="$OBSCURA_API_KEY"
```

HTTP 后端使用 `kubectl apply -k deploy`，然后单独应用 Tenant 示例：

```sh
kubectl apply -k deploy
kubectl wait --for=condition=Established crd/sandboxtenants.sandbox.obscura.dev --timeout=60s
kubectl apply -f deploy/base/tenant.yaml
kubectl wait --for=condition=Ready sandboxtenant/obscura-default --timeout=120s
```

`tenant.yaml` 不包含凭据明文，也不包含在 base Kustomization 中，须在 CRD 和
Secret 已存在后应用。它为现有示例的 `obscura-system` 命名空间建立一个测试 Tenant，
上限为 10 个沙箱、8 个并发执行和 4 个 PTY。增加租户时由管理员创建独立命名空间、
该命名空间内的 Runtime，以及不同名称和 namespace 的 SandboxTenant；凭据 Secret
仍放在认证命名空间。不要在已有 Tenant 上改变 namespace。

共享 Controller/Gateway 使用 ClusterRole 访问租户命名空间和集群级 Tenant；
Controller 还需 watch Secret 来更新凭据摘要。leader-election Lease 权限仍限于
系统命名空间。升级旧部署后，原先的 namespaced Role/RoleBinding 不会自动删除，
可在确认新绑定生效后由管理员移除。

后端服务通过自身 Helm / manifest
独立安装；Obscura 不重复部署或修改后端的调度器、DaemonSet 和宿主机资源。
Backend 类型必须选择已完成 provider contract 验收的 adapter。当前规划固定
AgentENV `v0.2.3`、OpenSandbox `release-1.1.1` 和 CubeSandbox `v0.7.3`；入口填写
对应集群 Service URL，并固定部署镜像到相同 release/commit。不要把 provider 名称
当作兼容性证明，先通过 [Runtime Contract v1](plans/2026-10-16-runtime-contract-v1.md)
的控制面和数据面契约测试。

自研 UDS 后端使用 `deploy/overlays/native`：先在一个 Linux 节点启动 Rust
节点，并且**仅给这一个节点**设置 `obscura.dev/runtime-node=true` 标签。
overlay 将 Controller / Gateway 固定到该节点并挂载 `/run/obscura`，以 UID 0
连接 0600 socket；它们不自行管理 Firecracker 或宿主网络。此拓扑不支持跨节点
高可用。Runtime 数据库和镜像需要持久化在节点上。

生产接入还需要分别验收宿主机的 Linux/KVM、Firecracker、TAP/nftables、
`ip_forward`、guest 镜像和本地持久文件系统。当前清单没有部署 Rust 节点，
也没有提供跨节点的安全传输、调度或数据库故障转移。节点数据库升级、备份和
恢复必须遵守 [Controller 契约](../runtime/CONTROLLER_CONTRACT.md)；隔离身份
的自动解除尚未设计成可执行流程。HTTP 后端还必须针对选定的 AgentENV、
CubeSandbox 或 E2B 版本验证发现/分页、创建和删除契约，不能凭类型名称认定兼容。

```sh
kubectl label node YOUR_LINUX_NODE obscura.dev/runtime-node=true
kubectl apply -k deploy/overlays/native
kubectl wait --for=condition=Established crd/sandboxtenants.sandbox.obscura.dev --timeout=60s
kubectl apply -f deploy/base/tenant.yaml
kubectl wait --for=condition=Ready sandboxtenant/obscura-default --timeout=120s
kubectl apply -f deploy/examples/obscura.yaml
```

最后一条命令是管理员直接创建 CR 的验证路径，不计入 Gateway 配额；
租户业务创建应改用下文的 Gateway API。

## Gateway API

所有业务请求需要 `X-API-Key`；`GET /healthz` 为公开探针。`/v1` 是 Obscura
自己的 API，返回异步生命周期状态。可通过 `kubectl port-forward` 访问集群服务。
Gateway Service 当前提供集群内 HTTP；不要直接暴露到公网。需要外部访问时，
应由受信任的入口提供 TLS、访问控制和限流。当前提供 Tenant 命名空间隔离及配额，
不提供 Tenant 内独立用户身份或细粒度角色。

```sh
kubectl -n obscura-system port-forward svc/obscura-gateway 9000:9000
curl -X POST http://127.0.0.1:9000/v1/sandboxes \
  -H "X-API-Key: $OBSCURA_API_KEY" \
  -H 'Idempotency-Key: demo-request-1' \
  -H 'Content-Type: application/json' \
  -d '{"runtimeRef":"obscura","templateRef":"dev","resources":{"vcpus":1,"memoryMiB":512,"diskMiB":2048},"network":{"allowEgress":false},"timeoutSeconds":300}'
```

- `POST /v1/sandboxes`：持久化 CR，返回 202、sandboxID 和 Location。
- `GET /v1/sandboxes/{sandboxID}`：返回生命周期状态和 Conditions。
- `DELETE /v1/sandboxes/{sandboxID}`：请求异步删除，finalizer 等待后端清理确认。
- `POST /v1/sandboxes/{sandboxID}/execute`：Ready 时向自研 Runtime 执行命令，
  请求包含 command、input、envVars、timeoutSeconds；返回 NDJSON 流。
- `GET|PUT /v1/sandboxes/{sandboxID}/files?path=/absolute/path`：读取或替换文件，
  上限 32 MiB，要求后端实现文件能力。
- `GET /v1/sandboxes/{sandboxID}/pty`：WebSocket PTY 会话，要求后端实现 PTY 能力。

沙箱创建前通过 Tenant status 的乐观锁预留配额，后端删除确认后才释放；
不确定的创建结果仍占用配额。执行和 PTY 通过有期限的会话租约限制并发。
能力未实现返回 501，配额耗尽返回 429，业务错误使用 JSON `error.code/message`。

同一 Idempotency-Key 与相同规格重复请求会返回同一个 CR；不同规格返回 409。
CR 被彻底删除后再使用同一 key 会创建新 UID，当前 API 没有永久请求去重账本。
命令有效期不超过沙箱有效期，执行内容、环境变量和输出不写入 CR。

## AgentENV / CubeSandbox 接入范围

旧版通用 HTTP 生命周期原型需要后端提供：

- `GET /v2/sandboxes` 的元数据过滤、running/paused 查询及分页。
- `POST /sandboxes` 接受 templateID、timeout 和 metadata。
- 实例查询和删除接口返回可确认的状态。

类型名称表示选择该 HTTP 配置，不保证每个上游版本都符合这些接口。AgentENV
v0.2.3 的 Connect-RPC execute 和 `/files` codec 已通过仓库内协议模拟；PTY、端口、
快照与 clone 尚未实现。完整接入要求和固定版本见
[Runtime Contract v1](plans/2026-10-16-runtime-contract-v1.md)。接入前固定上游
commit/tag，并验证上述契约；当前尚未对真实部署做烟测。
可以从 `deploy/examples/agentenv.yaml` 开始，CubeSandbox 配置使用
`type: cubesandbox` 和它的 CubeAPI Service URL。

HTTP 后端省略 resources，规格由上游模板决定。当前只接受明确允许出口网络
的配置，不支持默认拒绝或域名白名单；这些请求会失败而不会悄悄放宽策略。
AgentENV adapter 仍未通过真实部署准入，因此当前 `Registry.Open` 会 fail-closed，
经 Gateway 选择该类型仍返回 501。不能把仓库内 Connect framing 测试当作上游部署
已经可用。完整 E2B SDK 兼容仍需要另外实现平台响应、PTY 和端口路由。

HTTP 创建只授权一次 POST；重试用 `obscura_uid` / `spec_hash` 元数据找回实例。
创建结果不确定且查询不到实例时标记 CreationUnknown，禁止自动重新 POST。
删除也会保持 finalizer，直到能确认或人工审计清理。请求未发出前的崩溃也可能
进入这一保守状态，这保证不重建，但不保证所有中断都自动恢复可用。

## 故障与验证

Runtime 短暂断联保留绑定，标记 Unknown。实例丢失、Failed 或 Deleted 时不
自动替换有状态实例。自研 Runtime 重启会隔离无法确认归属的旧 VM，相关清理
需要节点隔离和人工审计，详见 `runtime/CONTROLLER_CONTRACT.md`。

```sh
make test
make test-envtest
cd control && go test -race ./internal/backend ./internal/controller ./internal/kubegateway
kubectl kustomize deploy
kubectl kustomize deploy/overlays/native
```

envtest 启动真实 API Server / etcd，验证 CRD 结构、CEL 不可变规则、状态写入
失败恢复和 finalizer；首次运行需能下载 envtest 二进制，也可通过
`KUBEBUILDER_ASSETS` 指向预装的文件。单元测试不启动真实 Firecracker。
Linux/KVM 和上游真实部署的端到端测试仍需在对应环境完成。

后续已在 Apple Silicon macOS 下载并运行 Kubernetes 1.35.0 envtest；
`make test` 与 `make test-controlplane` 通过，覆盖原有 API/CEL/恢复验证与新增
manager/Gateway/gRPC 模拟 Runtime 联调。之后又通过 Compose/Linux 容器测试和
Kind 镜像/部署/smoke；完整记录见测试指南。Linux/KVM 和真实上游后端烟测仍未验证。
