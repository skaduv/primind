# 代码提交与协作规范

本项目采用“负责人主导、AI 辅助开发”的流程：需求与验收条件 → 小分支开发 → 本地验证 → PR → CI 与代码审查 → 负责人合并。负责人决定业务优先级、验收和发布；AI 根据任务授权实施、测试和审查，不因完成开发就自行合并或发布。

## 1. 开始一项业务改动

动手前记录业务问题、预期行为、改动范围和可验证的验收条件。修复缺陷时给出复现步骤；权限相关改动同时覆盖“应该允许”和“应该拒绝”的访问。缺少环境或数据时注明依赖，不把推测写成已验证结果。

- 一项可独立审阅、测试和回退的成果对应一个短期分支和一个 PR；不混入无关重构。
- 分支使用 `codex/<简短任务名>`，例如 `codex/kb-permission`；无需额外维护长期 `develop` 分支。
- 开始前检查目录、分支、HEAD 和工作区。已有未提交成果必须保留；并行 AI 使用不同 worktree，不共写一个目录。
- 在干净工作区从最新 `origin/main` 开始；有依赖时记录实际基线，不重置他人的分支。

```bash
git status --short
git branch --show-current
git fetch origin main
git switch -c codex/kb-permission origin/main
```

上述命令以新任务为例；已有任务续作时继续使用原分支。

## 2. 提交内容与命名

PR 标题采用以下格式，合并时使用 **Squash and merge**，并以 PR 标题作为最终提交标题：

```text
type(scope): 具体改动
fix(auth): 修复共享撤销后仍可访问知识库
feat(retrieval): 增加知识库检索范围限制
ci: 完善 PR 检查门禁
```

`type` 可选 `feat`、`fix`、`docs`、`refactor`、`test`、`chore`、`ci`、`build`、`perf`、`revert`；`scope` 可省略。破坏兼容的改动用 `!`，例如 `feat(api)!: 调整检索响应结构`，并在 PR 中说明迁移方式。描述可以使用中文。

工作分支内允许修复、补测等中间提交，CI 不要求重写每个 WIP 提交。最终标题应说明业务或工程变化，避免“更新”“修改一下”。

只暂存本任务的明确路径，提交前检查差异：

```bash
git diff --check
git diff -- path/to/changed-file
git add -- path/to/changed-file path/to/related-test
git diff --cached
git commit -m "fix(auth): 修复共享撤销后仍可访问知识库"
```

示例路径需替换为实际文件；不要使用 `git add .` 混入其他任务。不得提交 `.env`、真实凭据、数据库备份、迁移数据、生产日志或含用户资料的截图。示例配置必须脱敏；需要留存的验证报告只保留必要且脱敏的证据。

## 3. 本地验证

按改动选择相关检查，记录命令、结果和未执行原因。以下入口来自当前组件工作流；本地定向检查不替代 PR 上的完整组件 CI。

| 改动 | 本地验证入口 |
| --- | --- |
| Go 服务端 | 对改动文件执行 `gofmt`，运行相关包的 `go test`；完整包范围与 `go vet`、构建要求见 [App 工作流](.github/workflows/app.yml) |
| 前端 | 在 `frontend/` 执行 `npm ci`，再从仓库根执行 `bash scripts/verify_frontend_pr.sh`；包含测试、类型检查和构建 |
| CLI | 在 `cli/` 执行 `go build ./...`、`go test -race ./...`、`go vet ./...`；另遵循 [CLI 协作说明](cli/AGENTS.md) |
| Go SDK | 在 `client/` 执行 `go test ./...`、`go vet ./...`、`go build ./...` |
| DocReader | 依赖、Python 测试及实际 gRPC 服务下的 Go 客户端测试见 [DocReader 工作流](.github/workflows/docreader.yml) |
| Anydoc / Helm | 分别按 [Anydoc 工作流](.github/workflows/anydoc.yml)和 [Helm 工作流](.github/workflows/helm.yml)验证 |

Go 版本以各模块 `go.mod` 为准，CI 的具体工具版本以工作流为准。Windows 上 Bash 脚本使用 Git Bash；不要把 Bash 的环境变量、管道或目录语法直接粘贴到 PowerShell。涉及真实服务的测试只使用任务明确允许的隔离资源，不自动读取生产连接配置。

可选安装已有本地 hook：在 Git Bash 中从仓库根运行 `bash scripts/install-git-hooks.sh`。它会设置 `core.hooksPath`，并可能自动格式化和重新暂存 Go 文件，提交前仍需检查暂存差异。本地 hook 可以被跳过，也可能受工具安装情况影响，不能充当服务器端门禁。

## 4. PR 与审查

使用 [PR 模板](.github/pull_request_template.md)，填写业务问题、改动与验收、验证证据、配置或数据影响、恢复办法。尚在开发时使用 Draft PR；完成本地验证后再转为待合并。

AI 审查应检查**实际 diff 和测试证据**，重点关注权限边界、异常与失败路径、跨组件兼容及数据生命周期。可以由另一 AI 会话独立审查，但 AI 评论不等同于另一位维护者的批准。记录审查发现、处理结果和未解决项；确实未审查时写 `NOT_RUN`，不伪造审查结论。

单人模式不要求第二个人批准，不设置会阻塞自己的 CODEOWNERS 审批。负责人在合并前确认：

- 业务范围与验收条件合理，实际差异不夹带其他任务。
- 最新提交对应的 `CI Gate` 通过，失败或取消没有被当作成功。
- AI 审查意见已处理；未执行的真实环境验收、风险和后续验证有明确记录。
- PR 标题适合作为 squash 提交标题，配置或数据库变更已有兼容与恢复说明。

## 5. CI 门禁与合并边界

[CI 工作流](.github/workflows/ci.yml)统一接收 PR，按变更路径调用相关组件检查，由 `CI Gate` 汇总。需要执行的检查失败、取消或意外跳过时，门禁不能通过；不涉及的组件可以跳过。纯文档 PR 仍检查提交规范和门禁本身。

仓库中存在工作流不等于 GitHub 已禁止绕过。远程启用步骤、单人分支规则和验证方式见 [CI 门禁启用说明](docs/CI_GATE_SETUP.md)。日常通过 PR 合并，不直接推送或强推 `main`；紧急修复也应保持小范围 PR 和验证记录。

`CI Gate` 证明本次所需的自动检查通过，不代表真实 RAG 业务链、性能或生产环境已验收。上传解析、检索问答、引用、跨空间权限、删除或撤权等业务验证，应按改动范围单独记录。真实验收不能执行时写 `NOT_RUN` 与原因，由负责人决定是否适合合并。

合并到 `main` 可能触发现有镜像构建与推送；这不等于已部署生产。数据库结构或数据变更不能假设通过切回旧镜像恢复，必须明确兼容条件、备份或修复办法。生产部署与恢复操作需要对应任务的授权。
