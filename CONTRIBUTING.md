# 协作与发布规范

`dev` 汇总功能分支并执行集成验证，`main` 保存通过检查的发布基线。
开发使用短分支，通过 PR 合入 `dev`，验证后再提升到 `main`。
当前是原型阶段；版本号表示代码版本，实际运行限制以 README 和 Runtime 契约为准。
项目文档使用中文，Agent 协作规则见 [AGENT.md](AGENT.md)。

## 分支

- 命名：`<type>/<slug>`，例如 `feat/runtime-adapter`、`fix/sandbox-timeout`。
- type 使用下表中的提交类型；slug 使用小写英文、数字和连字符。
- 一个功能分支聚焦一件事，确认不再使用后删除；`dev` 是长期集成分支。
- 开始新工作前更新 `dev`：`git switch dev && git pull --ff-only`。
  创建分支命令默认从当前 HEAD 开始，保留当前未提交内容；也可显式传 `--base origin/dev`。
- 不直接向 `main` 推送开发提交，不 force push 共享分支；本工具没有强推选项。

## Commit 与 PR 标题

格式：`type(scope)!: description`，scope 和 `!` 可省略，标题最多 100 字符。
描述可以用中文，使用动词说明具体变化；避免 `update`、`fix bug` 之类空泛标题。

| type | 用途 |
| --- | --- |
| `feat` | 新增能力 |
| `fix` | 修复缺陷 |
| `docs` | 文档 |
| `refactor` | 不改变行为的重构 |
| `perf` | 性能优化 |
| `test` | 测试 |
| `build` | 构建、依赖和打包 |
| `ci` | 持续集成 |
| `chore` | 工具和维护 |
| `revert` | 回退变更 |

推荐 scope：`control`、`runtime`、`guest`、`api`、`deploy`、`repo`。
例如 `feat(control): reconcile sandbox expiration`、`chore(repo): add git workflows`。
不兼容变更使用 `!`，正文增加 `BREAKING CHANGE: ...`，说明影响和迁移方式。
回退提交也使用规范标题，例如 `revert(runtime): undo snapshot restore`，正文记录原提交 SHA。

## 命令

需要 Git、Python 3.11+；PR 还需要 GitHub CLI，并提前执行 `gh auth login`。
在仓库根目录运行；任意 Git GUI/编辑器都可以配合这些脚本使用。
`.claude/commands/` 同时提供 `/branch`、`/commit-push`、`/pr`、`/release-tag`，
供支持这种 command 目录的客户端加载。

```sh
# 查看全部命令；可选启用本仓库的 commit-msg hook
make git-help
make git-hooks

# 新建分支（只使用本地 ref，不自动 fetch）
python3 scripts/git-workflow.py branch feat runtime-adapter

# 方式一：自行审查并暂存，然后提交 + 推送
git add -- docs/kubernetes.md
python3 scripts/git-workflow.py commit-push -m 'docs: clarify runtime setup'

# 方式二：指定路径；要求暂存区为空，防止混入已有暂存内容
python3 scripts/git-workflow.py commit-push \
  -m 'fix(control): handle sandbox expiration' \
  --paths control/internal/controller/sandbox_controller.go

# 提交成功但推送失败时，仅重试 push
python3 scripts/git-workflow.py push

# 按 PR 模板在仓库外准备正文，再创建 draft PR
cp .github/pull_request_template.md /tmp/obscura-pr.md
# 编辑 /tmp/obscura-pr.md，填入实际变更、验证结果与风险
python3 scripts/git-workflow.py pr \
  --title 'feat(runtime): add sandbox lifecycle' \
  --body-file /tmp/obscura-pr.md
```

`commit-push` 默认只提交已有暂存区，`--paths` 则暂存明确指定路径下的新增、修改和删除。
目录参数包含其下所有变化，使用前先审查 `git status` 和完整 diff。命令检查提交格式和
`git diff --cached --check`，不会自动运行产品测试；提交前按变更范围执行下面的验证。
推送只针对当前分支并设置 upstream，不附带 tag。

`pr` 脚本仍默认以 main 为目标；合入 dev 时使用 `gh pr create --base dev`。
该脚本要求工作区干净、HEAD 已推送且相对最新远端 main 有提交，显式使用 origin 仓库和当前分支。
默认创建 draft，`--ready` 可直接发起 review；已有同仓库、同 head/base 的 open PR 则返回其 URL，
不覆盖标题或正文。PR 命令不会提交或推送代码。原始 gh 的 `pr create --dry-run` 仍可能推送，
本流程不使用它做无副作用预览。

本地 hook 通过 `make git-hooks` 显式启用；已有 hooksPath 或默认目录中的自定义 hook 时拒绝覆盖。
这些仓库应自行整合，因为设置 hooksPath 会切换整个 hooks 目录。
停用本次本地设置：`git config --local --unset core.hooksPath`（仅在当前值为 `.githooks` 时执行）。
CI 同时验证 PR 标题和分支、主分支最新提交标题、版本一致性以及工作流脚本测试。

## 评审与验证

每次推送 `dev` 前必须完整执行 `make ci` 并确认通过；首次或依赖变化后先执行
`make dev-bootstrap`。远端 Actions 也执行 `make ci`，推送后检查对应提交的结果。
本地通过、远端运行中、远端通过是不同状态，交付时分别说明。

PR 描述说明具体问题、修改后的行为、验证结果，以及未验证项和风险。
涉及沙箱生命周期、隔离、网络、清理、协议时，说明权限与兼容性影响。
不在 PR 描述中声称未执行的测试通过。

| 变更范围 | 验证入口 |
| --- | --- |
| Go 控制面 | `make test-go`；涉及 reconcile 时补充 `make test-envtest` |
| Rust Runtime | `make test-rust` |
| guest runner | `make guest-test` |
| 跨语言协议 | `make proto-generate protocol-check test-go guest-test` |
| Git 工作流工具 | `make test-git-workflow` |
| 多模块变更 | `make test`，按需要补充 envtest / Linux KVM 验证 |

macOS 端到控制面联调使用 `make test-mac`；交互调试使用 `make dev-up`，
环境边界、依赖准备和故障注入见 [测试指南](docs/testing.md)。

产品测试通过不表示已完成真实 KVM、AgentENV 或 CubeSandbox 验收；记录实际验证边界。
Review 通过后 squash merge，最终 squash 标题沿用规范 PR 标题。

## 版本与 tag

- 项目版本记录在根目录 `VERSION`，当前为 `0.1.0`。
- 使用 SemVer：`vMAJOR.MINOR.PATCH`，候选版使用 `vX.Y.Z-rc.N`。
  patch 修复兼容问题，minor 新增兼容能力；稳定版的不兼容变更升 major。
  `0.x` 阶段不兼容变更升 minor，并明确迁移影响。
- 发布前在 PR 中一起更新 `VERSION`、Rust package version、guest 的
  `package.json` 和 `package-lock.json` 根包版本；Cargo.lock 如有变化一并提交。
- PR 合并且必要验证通过后，从与远端一致的干净 `main` 创建 annotated tag。
  tag 不自动生成 GitHub Release、构建产物或部署。
- 已发布 tag 不移动、不删除后重建；有问题发布下一个 patch。

```sh
python3 scripts/git-workflow.py check-version
python3 scripts/git-workflow.py tag v0.1.0          # 仅本地
python3 scripts/git-workflow.py tag v0.1.0 --push   # 发布该 tag；也可一步创建并推送
```

tag 命令会访问 origin，检查版本一致、main 与远端一致、远端没有同名 tag。
CI 检查版本 tag 为 annotated tag、版本匹配且属于 main 历史。

## GitHub 设置建议

仓库维护者可在 GitHub 设置 main ruleset：要求 PR 和 review、要求
`Git conventions / conventions` 检查通过、禁止 force push 和删除 main。
合并方式只启用 squash merge，默认使用 PR 标题；启用合并后自动删除分支。
tag ruleset 保护 `v*` 的更新和删除，只允许维护者创建。
这些是建议配置；本次文件建设不修改远端仓库设置。
