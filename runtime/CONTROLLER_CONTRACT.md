# Controller 接入契约

节点 gRPC 服务仍只监听本地 Unix domain socket，权限为 0600。Controller 必须通过
显式部署的本地代理或 sidecar 访问它。当前没有增加 TCP 监听端口，也没有提供远程
传输或认证方案。

## 持久身份

`CreateSandboxRequest.sandbox_id`（字段 5）是可选的调用方指定 UUID。提供该值时，
`spec_hash`（字段 6）不得为空。应使用 Kubernetes Sandbox UID，或从该 UID 和
明确的创建尝试派生的 UUID。未提供 `sandbox_id` 的请求保持旧版随机 ID 行为，
在创建响应不确定时不能安全重试。

节点保存实际模板、CPU、内存、磁盘、出站设置和调用方规格哈希的 SHA-256 指纹。
gRPC 入口还把 `max_runtime_nanos` 纳入指纹。request ID 仅用于追踪，不是幂等键。
域名白名单尚未实现，因此相关请求仍会被拒绝。运行时截止时间由 Controller 负责
强制执行。

SQLite 事务会在文件系统、网络或 VM 副作用发生之前预留身份、指纹和网络槽位。
成功的重试返回原有 Ready、Running 或 Paused 沙箱及其实际状态。不可变规格不同
时返回 gRPC AlreadyExists。已有 Creating、Failed 或 Deleted 身份返回
FailedPrecondition，绝不会静默再次启动。丢失响应后仍可用 GetSandbox 查询预留
身份。提交 Ready 状态失败时，Creating 保持可观察且必须通过 Delete 清理；
不能触发替代 VM。只要调用方仍可能重试这些 ID，就必须保留 Deleted 墓碑和指纹，
不得回收。

数据库 schema 版本保存在 `PRAGMA user_version`。当前版本为 1；启动时以单个事务
把旧版 `network_slot UNIQUE` 约束迁移为部分唯一索引，外键校验失败会回滚迁移并
拒绝启动。高于当前版本的数据库也会被拒绝，不能由旧版节点降级打开。网络槽位
范围是 1 到 16383，优先选最小空闲槽。只有 `Deleted` 且没有进程句柄的记录才
释放槽位；`Failed`、Creating 和重启后隔离的身份继续占用。墓碑与指纹仍保留，
槽位耗尽返回 gRPC `ResourceExhausted`，不会启动 VM。

Health 在 `HealthResponse.capabilities`（字段 3）中声明
`idempotent-create-v1`。依赖持久化创建语义的 Controller 应要求此能力，并在
超时和重启后保留选定的节点与 Runtime 绑定。

## 删除与观察

不存在或已经 Deleted 的身份可以成功删除。对已知的托管沙箱，节点先持久化
Failed，再停止 VM；随后持久化移除进程句柄，最后清理按身份确定的网络和磁盘资源。
失败时保留记录以便重试。清理时允许 TAP 设备或 nftables 表已经不存在；其他错误
会向上返回。同一守护进程中的 Creating 记录也能删除，包括 VM 已启动、Ready
尚未提交的间隙。具体 provider 可以通过预留的 UUID 停止进程。

报告原本 Ready、Running 或 Paused 的实例前，观察逻辑会检查守护进程拥有的子进程
句柄。进程已退出或丢失时，状态变为 Failed。这能发现进程死亡，但不是持续的
guest 健康探测。

## 重启隔离与运行限制

SQLite 存储通过生命周期独占的 advisory 文件锁（`<database>.lock`）防止第二个
守护进程隔离仍由存活节点拥有的记录。实现要求 Rust 1.89 或更新版本，并要求本地
持久文件系统正确支持 advisory 锁和 SQLite WAL 语义。

重新打开存储时，先前所有活跃或可能仍占用资源的记录都会被持久化隔离并报告
Failed。守护进程仍可服务其他身份，但不会接管旧进程、向持久化的 PID 发信号，
或宣称那些 VM 已 Ready。Create 不能替换隔离身份；Delete 返回
FailedPrecondition，不触碰其 VM、TAP、nftables 或磁盘资源。这也覆盖 VM 启动后、
具体进程句柄保存前发生崩溃的情况。

跨进程 VM 自动恢复和解除隔离尚未实现。运维人员必须先隔离旧节点与 Runtime 进程，
确认相关 VM 已停止，并清理相应资源，然后再修复记录。不能仅凭 PID 或 socket
消失就解除隔离。保留身份墓碑，防止重放创建第二台 VM。守护进程异常退出也可能
留下本地 gRPC socket 路径；确认没有进程拥有它之后，重启前移除陈旧 socket。
当前版本不会自动删除已存在的端点。

节点本地数据库是幂等保证的边界：重试被发送到使用另一数据库的节点时，不会去重。
创建中断后不保证 exactly-once 可用性；安全保证是绝不自动重新启动已有身份。

## 数据库备份与恢复

生产部署必须把节点 SQLite 数据库、WAL 和锁文件放在同一支持本地文件锁的持久
文件系统上；不要放在不保证 SQLite WAL/锁语义的共享存储上。升级前先停止节点
服务并确认旧进程已退出，再对数据库做一致性备份。在线备份应使用 SQLite backup
API；不能在服务运行时仅复制主 `.sqlite` 文件，因为最近提交可能仍在 WAL 中。

恢复旧备份会丢失备份之后创建的身份墓碑，可能让相同 UID 在另一数据库或恢复后
再次启动。恢复之前必须隔离该节点及相关 VM，并与 Kubernetes 中仍存在的绑定、
创建意图和外部资源逐一核对；无法证明安全时保持隔离，不允许自动创建或删除。
当前没有实现跨节点数据库复制、自动故障转移、备份协调、孤儿资源扫描或隔离解除。
因此 SQLite 是单节点持久身份存储，不是高可用控制面数据库。

## 验证范围

单元测试使用真实 SQLite 存储和故障注入，覆盖创建响应丢失、不可变规格变化、
Ready 提交失败、进程死亡、Creating 删除、停止失败与重试、清理资源缺失、
存储独占和重新打开节点后的隔离。它们不启动真实 VM；实际集成验收需要 Linux、
KVM、Firecracker 和配置好的 guest 镜像。
