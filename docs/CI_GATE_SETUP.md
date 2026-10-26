# 第一批：启用代码提交与 CI 门禁

适用于一位负责人配合 AI 开发。提交规范见 [CONTRIBUTING.md](../CONTRIBUTING.md)，PR 使用 [模板](../.github/pull_request_template.md)。本批不引入部署工作流，也不要求第二个人批准。

## 1. 仓库文件提供的能力

- `CI` 工作流接收所有 PR 的打开、更新、重新打开、标题/正文编辑及转为待审查事件。没有顶层路径过滤；文档 PR 也会出现 `CI Gate`。
- 准备步骤验证工作流语法、门禁脚本测试和当前 PR 标题，再用完整 Git diff 选择组件。包含删除、重命名前后路径，避免 API 文件列表截断；主干分叉之后的 PR 改动使用 merge-base 比较。
- 复用现有 App、Frontend、DocReader、Anydoc、CLI、Helm 和增量 Go lint；新增独立 Client SDK 检查。原有 main push 和手动触发入口保留，PR 组件检查只由统一入口调度。
- `CI Gate` 使用 `always()` 汇总：准备步骤失败、输出缺失、所需组件失败/取消/意外跳过均不通过；未涉及的组件可以跳过。普通文档 PR 不启动 Go、Python、Docker 或 Rust 重型检查。
- PR 检查仅需 `contents: read`，不继承部署凭据，不调用镜像推送或生产部署。现有可选 `CLI E2E` 仍按原标签/手动规则独立运行，不属于本批必过门禁；它的跳过不能当成真实业务验收。

### 组件选择

| 变更 | 必需检查 |
| --- | --- |
| 普通根目录/`docs/` 文档、PR 模板 | 工作流语法、门禁自测、PR 标题、汇总 |
| `internal/`、`cmd/`、配置、迁移及未识别的源码/配置 | App；Go 改动增加根模块增量 lint |
| `frontend/`、前端构建/代理测试脚本 | Frontend（含 UI 镜像与路由测试） |
| `docreader/` 及其 Dockerfile | DocReader；Go 改动增加根模块增量 lint |
| `third_party/anydoc-go/`、内置解析器及构建脚本 | App + Anydoc（含现有 cargo audit） |
| `cli/` | CLI；Go/依赖改动增加 CLI lint |
| `client/` | Client SDK + CLI；Go/依赖改动增加两模块 lint，因为 CLI 使用本地 SDK |
| 根 `go.mod` / `go.sum` | App + DocReader + Anydoc + 根模块 lint |
| `testdata/` | App + DocReader |
| `helm/` | Helm |
| 工作流、`scripts/ci/` 或共享 lint 配置 | 全部组件和全部 lint 模块 |

路径规则集中在 [changes.mjs](../scripts/ci/changes.mjs)。运行时嵌入的 Markdown、生成的 Swagger 数据、第三方声明不会因为扩展名像文档而自动免检。各组件实际覆盖范围以工作流中的命令为准；例如 App 构建不等于所有 Docker 镜像均已构建。

## 2. 先验证工作流，再启用远程强制规则

**本地文件存在不等于远程规则已经生效。** [main.json](../.github/rulesets/main.json) 是可导入的规则草案，初始 `enforcement` 为 `disabled`，以便先核对现有规则、检查名称和账号能力。只有远程改为 Active 并验证后，才能记为“门禁已启用”。

1. 将本批分支提交为 PR，目标为 `main`，标题例如 `ci: 建立单人 AI 开发提交门禁`。先确认最新提交的 `CI Gate` 实际运行成功。不要用 `[skip ci]` 跳过检查。
2. 在 GitHub 的 Settings → Rules → Rulesets 检查现有仓库及组织规则；同时检查旧 Branch protection。先保存现状，更新已有对应规则优先，不盲目叠加或删除其他保护。
3. 无对应规则时，从 Rulesets 的导入入口导入 `main.json`；它初始禁用。已有规则时，按下一节逐项调整。确认适用于 `refs/heads/main`。
4. 必过检查选择实际运行产生的 **`CI Gate`**，来源选择 **GitHub Actions**。模板不猜测 App ID；UI 中核对来源。原先按路径触发的 App/Frontend 等旧必过项，只有确认已被本门禁覆盖后才移除，其他独立保护保留。
5. 先在 Settings → General → Pull Requests 确认 **Allow squash merging** 已开启，再把 squash 提交默认标题设为 PR 标题；规则允许 squash 不会自动开启仓库级 squash。核对单人设置后，将规则改为 **Active**。合并前再次核对标题，其他仓库合并方式设置需协调现有流程。
6. 按第 4 节验证实际阻断和恢复。检查正常后由负责人决定合并。合并到 main 可能触发现有镜像构建/推送；本批不改生产发布流程。

GitHub 对私有仓库的规则功能受账号方案限制；需要仓库管理或规则编辑权限。若 UI 提示当前方案不可用，应记录实际限制；不能把仅在 PR 中显示的绿色检查当作强制保护。规则能力与导入方法见 [GitHub 官方说明](https://docs.github.com/en/repositories/configuring-branches-and-merges-in-your-repository/managing-rulesets/creating-rulesets-for-a-repository)。

## 3. 单人模式的规则值

| 设置 | 值及原因 |
| --- | --- |
| Require a pull request before merging | 开启，日常变更通过 PR |
| Required approvals | **0**，负责人可以合并自己的 PR |
| Require review from Code Owners | 关闭，不引入单人审批死锁 |
| Require approval of the most recent push | 关闭，不要求另一人批准最后一次推送 |
| Resolve review conversations | 开启，负责人处理并解决审查意见 |
| Required status checks | `CI Gate`，来源 GitHub Actions |
| Require branches to be up to date | 开启；main 更新后先同步再重检 |
| Allowed merge methods | Squash；最终标题沿用已检查的 PR 标题 |
| Linear history / Block force pushes / Restrict deletions | 开启 |
| Bypass list | 空；不添加日常绕过账号或机器人 |

不配置 merge queue；若以后启用，需要同时支持 `merge_group` 事件和相应比较基线，本批不能直接接队列。AI 审查属于人工审阅辅助，没有伪造为一个外部审批人。负责人仍有修改规则的管理能力，规则不能代替变更记录。

## 4. 验证与完成标准

本地检查可在 PowerShell 或 Git Bash 执行（需可用 Node.js）：

```text
node --test scripts/ci/*.test.mjs
git diff --check
```

CI 自动使用固定版本 actionlint 及 SHA-256 校验其下载物，检查 GitHub Actions YAML/表达式/复用接口；不把它宣称为 Bash/Python 完整静态检查。需要本地复核时使用官方 actionlint，并执行 `actionlint -shellcheck= -pyflakes=`。

远程验证记录 PR URL、head SHA、run URL 和结果：

| 场景 | 预期 |
| --- | --- |
| 纯文档 PR + 合法标题 | 轻量检查和 CI Gate 通过，无关组件跳过 |
| 把 PR 标题改为不符合格式，再改回 | edited 触发重检；先失败阻断，再恢复通过 |
| 修改某组件，使其测试失败 | 对应组件失败，CI Gate 失败，不能合并；修复后重检 |
| 所需组件取消或意外跳过 | 不能获得通过的 CI Gate；不能合并 |
| 删除 Go 文件、跨模块移动文件、修改 client | 对应模块及依赖组件被选中，lint 不漏检 |
| main 前进而 PR 落后 | 提示更新分支，更新后重新检查 |
| 负责人自己的 PR 全部满足 | 可以自己 squash 合并，不等待第二个人批准 |

负例在临时 PR/测试分支验证，恢复后才能合并；不向 main 推送错误代码来演示保护。单元测试覆盖失败与取消结果的判定，但不能代替 GitHub 端规则生效验证。

完成状态分开记录：①文件与本地验证；②GitHub 工作流真实运行；③远程规则已启用且阻断验证通过。本批组件 CI 不证明登录、权限、上传解析、检索问答、删除撤权、性能或生产环境已验收。

## 5. 失败排查与维护

- 标题失败：按提示修改 PR 标题，编辑事件会重新运行。不要只修改本地 commit 后等待标题自动变化。
- 组件失败：打开对应 job 的原始日志，修复根因后更新 PR，不用 `continue-on-error` 或删必过项掩盖失败。
- 检查缺失或一直 Pending：确认 PR 工作流已运行、状态名称/来源正确、没有遗留的路径过滤必过项、没有 `[skip ci]`；参见 [GitHub 必过检查排查](https://docs.github.com/en/pull-requests/how-tos/merge-and-close-pull-requests/troubleshooting-required-status-checks)。
- Go lint 只阻断新增问题，历史存量另立任务处理；这不是对全库零问题的承诺。
- 重命名组件、修改路径或门禁自身时，同时更新路由、汇总依赖和测试。可复用工作流的并发组必须保持组件前缀，避免与调用方互相取消；参见 [复用工作流限制](https://docs.github.com/en/actions/reference/workflows-and-actions/reusing-workflow-configurations)。
- 回退本批工作流前，先确认远程所需状态如何继续产生；直接删掉 `CI Gate` 而保留 required check 会阻塞后续 PR。修复优先通过最小 PR；确需临时调整规则时由负责人记录原因、原值和恢复条件。
