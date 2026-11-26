# BF-02 OpenSearch 检索响应体积治理：开发与验收 spec

| 项目 | 内容 |
| --- | --- |
| 关联缺陷 | [快速问答多库检索时 OpenSearch 响应超限](../../bug/2026-11-24-快速问答OpenSearch响应超限.md) |
| 优先级 | P0 |
| 状态 | 进行中：T0～T5、T8 已在 worktree 分支 `claude/bf-02-opensearch-response-size` 实现并提交，已变基到 `main@bb230e1`（含 BF-01），在变基后的代码上重新通过第 6.1 节单元测试和第 6.2 节本机集成测试，尚未推送、未合并；T4b 待排期，T7 部分完成，生产复测（AC1、AC2）待部署。记录见第 10 节 |
| 基线 | `main@746189f`；现有改动在 `C:/Users/11756/.codex/worktrees/c596/primind` 的 `retrieve.go` 和 `runtime_contract_test.go`（+82 行）。该工作树另有与本任务无关的未提交改动，移植时只取这两个文件 |
| 日期 | 2026-11-25（北京时间） |
| 修订 | 2026-11-25：按代码核对结果修订第 1～9、11 节，新增第 12 节（16 MiB 上限的来历）；2026-11-26：变基到 `main@bb230e1` 后复测，结果与变基前一致 |

---

## 1. 根因与长期风险

单次检索的响应体积约等于"候选条数 × 每条大小"：

- **候选条数有上限**：`min(500, max(召回数 × 5, 50) × 库数)`。[候选数计算](../../../internal/application/service/knowledgebase_search.go#L215) 召回数为 10 时，约 10 个库就达到 500 的上限；之后库数再增加，候选条数也不再增加。向量和关键词两路各发一次查询，各自返回最多 500 条。
- **每条大小**取决于正文长度，旧查询还会把 4096 维向量一起返回。这是唯一会持续增长的变量。
- **正文大部分是冗余的**：进入下游的正文由 `processSearchResults` 从 PG 重新读取。[重新读取](../../../internal/application/service/knowledgebase_search_results.go#L36) OpenSearch 返回的 `content` 只用于填充 `MatchedContent`，并且 500 条候选最终只有约 `MatchCount` 条进入下游。[截断](../../../internal/application/service/knowledgebase_search.go#L144)
- **客户端上限**默认 16 MiB，超出就报 `ErrResponseTooLarge`。[默认上限](../../../internal/application/repository/retriever/opensearch/runtime.go#L31) 生产环境没有修改这个值的配置入口。另外，解析层还有三处写死的 16 MiB `LimitReader`（检索、批量更新、迁移）。上限的来历和能否调整见第 12 节。
- **失败不降级**：
  - 同一嵌入模型的库会合并成一次 `HybridSearch` 调用。[合并检索](../../../internal/application/service/chat_pipeline/search.go#L484)
  - 扇出层任意一组失败，会取消其余各组，并统一折成 `VectorStoreUnavailableError`。[扇出失败处理](../../../internal/application/service/knowledgebase_search_fanout.go#L94)
  - 所有组都失败时，整次问答失败。[问答失败处理](../../../internal/application/service/chat_pipeline/search.go#L122)

字段过滤只去掉了向量这一项冗余。按满 500 条估算，平均每条正文超过约 32 KiB 就会再次超限（缺陷文档里 40 KiB 正文的模拟已经复现）。能否再次超限，取决于切片的最大长度（见 T0）。

## 2. 目标与非目标

**目标**

- G1：检索响应只返回必需字段（合并已完成的字段过滤），并让解析层跟随客户端实际的上限，不再另写一个 16 MiB。
- G2：响应体积与正文长度脱钩：检索阶段不返回正文，只为截断后进入下游的少量候选补充 `MatchedContent`（开关控制，是否实施以 T0 结论为准）。
- G3：超限时在驱动内自动缩小返回条数重试，返回部分结果，不再让整次问答失败。
- G4：每次检索都记录响应字节数，接近上限时能提前发现。
- G5：长正文负载下问答仍然成功，并测得内存和耗时数据。

**非目标**

- 不修改索引 mapping，也不重建索引。
- 不调整 16 MiB 默认上限，本期也不新增调整它的配置项（理由见第 12 节）。
- 不改动召回打分、重排算法和阈值语义。
- 不改变扇出层"存储级失败即整体失败"的约定（`TestRetrieveFromStores_EngineLevelErrorStillFails`）。降级在驱动内完成。

## 3. 兼容性约束

- C1：检索结果的字段、排序和分数与改造前一致。
- C2：正文延后读取由配置开关控制，默认关闭；关闭时完全走现有路径。
- C3：`RetrieveEngine` 接口签名不变。新增能力只能通过可选接口（类型断言）提供，其他后端不实现也能正常工作。
- C4：`copy.go` 的复制扫描需要完整文档（含向量）。它走独立的 `copyScanBatch`，不经过 `search()`，改动 `search()` 不影响它；新增改动也不得套用到复制路径上。
- C5：检索阶段不论是否取正文，都必须保留服务层用到的全部字段：`chunk_id`、`knowledge_id`、`knowledge_base_id`、`source_id`、`source_type`、`tag_id`、`is_enabled`。其中 `source_type` 是图片召回过滤的依据。[图片过滤](../../../internal/application/service/knowledgebase_search_image.go#L146)

## 4. 设计

### 4.1 第一步：合并字段过滤并修正解析上限（已完成大部分，待合并）

- `Repository.search` 增加 `_source_includes`，只取 `hit` 结构体解码的 8 个字段。
- 字段清单由 `hit.Source` 的 json 标签通过反射生成，不再手写；如果保留手写清单，就在旁边加测试，断言两者集合完全一致。
- 三处写死 16 MiB 的 `LimitReader` 改为读取客户端实际的上限（`responseCap`），或直接去掉，因为传输层已经按上限读过一次：
  - [retrieve.go:138](../../../internal/application/repository/retriever/opensearch/retrieve.go#L138)
  - [bulk_update.go:113](../../../internal/application/repository/retriever/opensearch/bulk_update.go#L113)
  - [move.go:53](../../../internal/application/repository/retriever/opensearch/move.go#L53)

### 4.2 第二步：正文延后读取（开关控制，视 T0 结论实施）

开关：环境变量 `OPENSEARCH_DEFER_CONTENT`，默认 `false`。

1. **检索阶段**：查询条件不变，`_source_includes` 使用 4.1 的清单去掉 `content`，满足 C5。500 条候选的响应预计在 200 KiB 左右，以实测为准。
2. **截断**：沿用服务层现有的去重、RRF 融合、FAQ 后处理和 `MatchCount` 截断，不做改动。
3. **补充 `MatchedContent`**：在 `HybridSearch` 截断之后、`processSearchResults` 之前，只为留下的候选补充。按 T0 的结论二选一：
   - **方案 A（优先）**：用 PG 数据重建。普通切片的索引正文是"标题 + 换行 + 正文"。[索引正文](../../../internal/application/service/knowledge_index_content.go#L12) 生成问题和 FAQ 行从各自的元数据中取得。这样不需要再访问 OpenSearch。
   - **方案 B**：新增可选接口（例如 `MatchedContentFetcher`），OpenSearch 用 `ids` 查询按 chunk_id 读取 `content`。数量约等于 `MatchCount`，单批读取即可，不需要分批或二分重试。
4. **一致性**：下游已经通过 `listChunksByIDWithShared` 从 PG 重新读取切片。开发前确认这一路径会过滤已删除和已禁用的切片；如果不过滤，就在这一步补上。
5. **保序**：`MatchedContent` 只是附加字段，不改变顺序和分数。

### 4.3 第三步：超限降级（不依赖开关）

- **驱动内重试**：`vectorRetrieve` 和 `keywordsRetrieve` 收到 `ErrResponseTooLarge` 时，把 `size` 减半后重试，最多 3 次（500 → 250 → 125 → 62）。
  - 向量查询保持 `k` 不变，只减小 `size`，返回的就是原结果的前缀，满足 C1。这需要 `buildKNNQuery` 拆开 `k` 和 `size` 两个参数。
  - 关键词查询减小 `size` 本身就返回原排序的前缀。
- **部分结果**：重试成功时，在返回的 `RetrieveResult.Error` 中标记"结果被截断"。`CompositeRetrieveEngine` 和 `retainPartialResults` 已经支持"带错误的部分结果"，结果会被保留。[部分结果约定](../../../internal/application/service/retriever/composite.go#L34) 降到最小 `size` 仍然超限时，返回原错误，按现有约定处理。
- **前端提示（T4b，单独排期）**：目前部分失败的信息只写进日志，`HybridSearch` 的返回值传不到流水线。需要新增传递通道（例如在 context 中收集检索诊断，或在 `ChatManage` 上加标记），再在回答的检索步骤里显示"部分知识库检索结果不完整"。文案需要产品确认。

### 4.4 第四步：可观测性

- 每次 OpenSearch 检索打一条结构化日志：`request_id`、索引、库数、`size`、命中数、响应字节数、耗时、重试次数、是否截断。字节数在 `search()` 中用计数 reader 统计。
- 响应字节数超过**客户端上限的 50%**（默认 8 MiB）时打 WARN。
- 问答级别汇总：检索组数、失败组数、总候选数、总字节数。
- 用测试确认 SDK 是否会重试 `ErrResponseTooLarge`（可通过 `requestTrace.attempts` 计数）。如果会重试，就把它标记为不可重试。

### 4.5 第五步：整次问答预算（推迟）

典型场景只有一个存储组，向量和关键词共两次查询，单次问答预算基本没有可拦截的查询，也限制不了进程内存。进程内存的上界由进程级 `RequestBudget` 决定：16 个并发许可 × 响应上限，默认约 256 MiB 缓冲，另有 JSON 解码开销（见第 12 节）。本期在负载测试中记录实际峰值；如果超出部署内存预算，再另开任务评估降低并发许可或上限。

### 4.6 其他后端排查（只排查，不改）

- Elasticsearch v8 的部分查询已经使用 `Source_` 过滤（[repository.go](../../../internal/application/repository/retriever/elasticsearch/v8/repository.go#L510)），需要确认检索主路径是否同样过滤。
- 其他后端逐个确认"检索时会不会把向量一起读回来"，结果记在本文第 10 节，有问题的另开缺陷单。

## 5. 开发任务

| 编号 | 任务 | 主要文件 | 依赖 | 完成标准 |
| --- | --- | --- | --- | --- |
| T0 | 预检：切片最大长度；`MatchedContent` 的全部使用方；方案 A 能否完整重建；PG 读取是否过滤禁用切片；SDK 是否重试超限 | 切片配置、`knowledgebase_search_results.go`、`knowledge_faq.go`、`message_files.go` | 无 | 结论写入第 10 节，并据此决定 T5 是否实施、采用 A 还是 B |
| T1 | 移植字段过滤和测试到干净分支，并修正三处 `LimitReader` | `opensearch/retrieve.go`、`bulk_update.go`、`move.go`、`runtime_contract_test.go` | 无 | 分支只含本任务改动，测试通过 |
| T2 | 字段清单由结构体标签生成（或一致性测试） | `opensearch/` | T1 | 故意删掉一个字段时编译或测试失败 |
| T3 | 检索日志与 50% 告警 | `opensearch/retrieve.go`、`runtime.go` | T1 | 单元测试断言日志字段 |
| T4 | 驱动内缩小 `size` 重试，返回部分结果 | `opensearch/retrieve.go`、`query.go` | T1、T3 | 模拟超限时返回结果前缀并标记截断，不报错 |
| T4b | 部分结果提示传到前端 | `knowledgebase_search*.go`、`chat_pipeline/search.go`、前端 | T4 | 模拟截断时检索步骤显示提示 |
| T5 | 正文延后读取（开关控制） | 服务层 `knowledgebase_search.go`；方案 B 另加 `opensearch/` | T0、T1 | 开关打开和关闭时，结果的 ID、顺序、分数、`MatchedContent` 完全一致 |
| T7 | 负载测试 | 测试脚本、本文第 10 节 | T4（T5 如实施） | 产出第 6.4 节的数据表 |
| T8 | 其他后端排查记录 | 本文第 10 节 | 无 | 每个后端都有结论 |

T1 应尽快单独合并上线，后续任务不阻塞它。T6（问答预算）已推迟，见 4.5。

## 6. 测试计划

### 6.1 单元测试（WSL 中运行）

```bash
go test ./internal/application/repository/retriever/opensearch/ -count=1
go test ./internal/application/service/retriever/ -count=1
go test ./internal/application/service/chat_pipeline/ -count=1
go test ./internal/application/service/ -run 'Search|Fanout|Image|FAQ' -count=1
```

| 用例 | 断言 |
| --- | --- |
| `TestRetrieveFiltersLargeUnusedSourceFields`（已有） | 4096 维命中在 1 KiB 上限下不再超限 |
| 字段清单 | `_source_includes` 与 `hit` 的 json 标签集合相等 |
| 解析层跟随上限 | 测试中通过 `WithRuntimeOptions` 把上限设为 32 MiB 时，20 MiB 的响应能正常解析（生产没有这个配置，只验证解析层不再写死） |
| 超限缩小重试 | 第一次超限、第二次成功：返回原排序的前缀，`RetrieveResult.Error` 标记截断，上层保留结果 |
| 向量前缀 | `k` 不变、`size` 减半时，结果是原结果的前缀 |
| 重试到底仍超限 | 返回 `ErrResponseTooLarge`，行为与现状一致 |
| SDK 不重复下载 | 一次超限只产生一次传输尝试 |
| 正文延后一致性 | 同一组模拟数据（含图片行、生成问题行、FAQ 行），开关打开和关闭时结果完全一致 |
| 禁用切片 | 两次读取之间被禁用的切片不进入结果 |

### 6.2 集成测试（本机 WSL 的 OpenSearch 2.19.3）

- 建 7 个测试库，每库 200 个片段，正文 2～40 KiB 随机分布，使用 4096 维向量，包含图片行和生成问题行。
- 分别在开关关闭和打开时执行同一问题，对比召回结果，并记录响应字节数和耗时。

### 6.3 端到端回归

- 复现缺陷文档中的三条路径：内置快速问答默认 7 库、共享「快速问答-中心制度」6 库、内置快速问答 `@` 指定 6 库。三条路径都要能召回、重排并正常引用。

### 6.4 负载测试

响应体积由"每条正文长度 × 返回条数"决定，库数超过约 10 个以后不再影响体积，所以测试矩阵以正文长度为主：

| 场景 | 库数 | 每条正文 | `MatchCount` | 记录项 |
| --- | --- | --- | --- | --- |
| 正文梯度 | 10 | 8 / 32 / 64 KiB | 10 | 每次响应字节数、是否触发截断、总耗时、引用结果 |
| 召回数梯度 | 10 | 32 KiB | 10 / 50 | 同上 |
| 库数对照 | 1000 | 8 KiB | 10 | 同上，并记录 1000 个 ID 的过滤条件对耗时的影响 |
| 并发 | 10 | 32 KiB | 10 | 16 / 32 路并发问答下的应用内存峰值，对照 Pod 1 GiB 上限 |
| 请求取消 | 100 | 8 KiB | 10 | 中途取消后，确认没有遗留的 goroutine 或连接 |

## 7. 上线流程

1. **第一批（T1～T3）**：字段过滤、`LimitReader` 修正、字段清单、日志。不需要开关，风险最低，尽快部署。部署后按第 6.3 节复测，并观察 3 天的响应字节数日志。
2. **第二批（T4、T4b）**：驱动内降级默认生效；T4b 在文案确认后随后上线。
3. **第三批（T5，视 T0 结论）**：以关闭状态部署，在预发环境打开并跑第 6.2～6.4 节测试，确认结果一致后，再在生产打开。
4. **回滚**：第三批关闭开关即可；第一、二批直接回退版本，没有数据副作用，也不涉及迁移。

## 8. 验收标准

| 编号 | 场景 | 预期 | 验证方式 |
| --- | --- | --- | --- |
| AC1 | 生产环境用子账号走内置快速问答默认 7 库，问缺陷文档里的原问题 | 正常回答并带引用，不出现 `response exceeds byte cap` | 手工 + 日志 |
| AC2 | 共享 6 库和 `@` 指定 6 库两条路径 | 召回、重排、引用正常，与修复前一致 | 手工 |
| AC3 | 检索日志 | 每次检索都有响应字节数；超过客户端上限 50% 时有 WARN | 日志检查 |
| AC4 | 模拟单次查询超限 | 问答成功，返回原排序的前缀；T4b 上线后显示结果不完整的提示 | 单元测试 + 预发手工 |
| AC5 | 正文延后读取开关打开 | 与关闭时相比，召回 ID、顺序、分数、`MatchedContent` 一致；500 条候选的检索响应小于 512 KiB（实测值记入第 10 节） | 集成测试 |
| AC6 | 64 KiB 正文、10 库 | 问答成功，或明确标记结果被截断，不出现未处理的错误；内存峰值和耗时记录在第 10 节 | 负载测试 |
| AC7 | 回归测试 | 第 6.1 节命令全部通过，附命令输出 | 测试日志 |

## 9. 长期运行保障

- 每周查看一次检索日志中响应字节数的 P95 和最大值。接近上限的 50% 时，优先打开正文延后读取，而不是调高上限。
- 如果以后确实要调高上限，必须同时核算"并发许可数 × 上限 × 约 2 倍解码开销"与应用 Pod 内存上限，并接入正式的配置项（见第 12 节）。
- 新增检索字段时，只改 `hit` 结构体（字段清单自动生成，或由 T2 的测试强制同步）。
- 新增检索后端时，按第 4.6 节的检查项确认不会把向量一起读回来。

## 10. 记录区（开发过程中填写）

记录时间 2026-11-25。实现在分支 `claude/bf-02-opensearch-response-size`（worktree `.claude/worktrees/bf-02-opensearch-response-size`），开发基线 `main@746189f`。2026-11-26 已提交并变基到 `main@bb230e1`（含 BF-01，两者改动的文件不重叠），在变基后的代码上复测：第 6.1 节 4 条命令、全模块构建、lint 和第 6.2 节集成测试全部通过，数据与下表一致。**尚未推送、未合并**。

### 10.1 T0 预检结论

| 检查项 | 结论 | 依据 |
| --- | --- | --- |
| 切片最大长度 | 初始化接口的 `chunkSize` 上限为 10000 字符，中文 UTF-8 约 30 KB，再加标题。500 条候选约 15 MB，已逼近 16 MiB。T5 需要实施 | `internal/handler/initialization.go:200` |
| `MatchedContent` 来源 | 唯一来源是 `buildChunkIndex` 读取的 `IndexWithScore.Content`。截断之前，融合、FAQ 后处理和图片过滤都不读 `Content` | `knowledgebase_search_results.go:128` |
| `MatchedContent` 使用方 | FAQ 的 `MatchedQuestion`、资源句柄授权检查、存储 URL 改写、引用来源清空、追问建议的摘要兜底 | `knowledge_faq.go:1069`、`access/message_files.go:166`、`storageurl/request.go:104`、`chat_pipeline/citation_sources.go:160`、`message_suggestion.go:725` |
| 方案 A 还是 B | **采用方案 B**。OpenSearch 写入时 `_id = chunk_id`，按 ID 读回的正是检索命中的那一行，结果按构造一致。方案 A 需要按行类型（普通切片、生成问题、FAQ、图片）分别重建，并依赖写入规则（R3） | `bulk_contract.go:197`、`crud.go:71` |
| PG 读取是否过滤禁用切片 | 会过滤。`assembleSearchResults` 经 `isSearchableChunk` 去掉禁用切片和 `index_status` 为 processing/failed 的切片，无需补充 | `knowledgebase_search_results.go:372`；新增测试 `TestChunkDisabledAfterRetrievalIsDropped` |
| SDK 是否重试超限 | 不重试。opensearch-go v4.6.0 只在 `io.EOF` 和 `net.Error` 时重试，`ErrResponseTooLarge` 不在其中。测试用 `RequestBudget` 的 attempts 计数确认：一次请求只有一次传输尝试 | SDK `opensearchtransport.go:336-344`；测试 `TestRetrieveShrinksSizeOnOversizedResponse`、`TestRetrieveShrinkGivesUpWithOriginalError` |

**附带发现（另开缺陷，不在本期范围）**：OpenSearch 以 `chunk_id` 作为文档 `_id`，而生成问题行与原切片的 `chunk_id` 相同（`chunk.go:810`、`knowledge_process.go:2508`）。同一索引中后写入的行会覆盖先写入的行，每个切片在 OpenSearch 中只保留一行。

### 10.2 实施情况

| 编号 | 状态 | 实现 |
| --- | --- | --- |
| T1 | 完成 | 移植了 codex 工作树的字段过滤和 `TestRetrieveFiltersLargeUnusedSourceFields`。三处 `LimitReader` 改为读取 `responseCap(client)` |
| T2 | 完成 | `hitSourceFields` 由 `hit.Source` 的 json 标签反射生成。`TestHitSourceFieldsCoverServiceFields` 断言其与 C5 的 8 个字段集合相等，删掉字段即失败 |
| T3 | 完成 | 每次检索写一条结构化日志，字段为 `request_id`、`index`、`retriever`、`kb_count`、`top_k`、`size`、`hits`、`response_bytes`、`response_cap`、`elapsed_ms`、`retries`、`truncated`。超过上限的 50% 时打 WARN。每次问答另记一条 `retrieval summary`，包含组数、是否失败、候选数、请求数、总字节数、截断结果集数 |
| T4 | 完成 | `buildKNNQuery` 拆分 `k` 和 `size`。超限时 `size` 减半重试，最多 3 次。成功后 `RetrieveResult.Error` 标记 `ErrResultsTruncated`，上层保留部分结果 |
| T4b | **未做** | 按第 4.3 节单独排期，文案待产品确认 |
| T5 | 完成（默认关闭） | `OPENSEARCH_DEFER_CONTENT`（默认 false）。新增可选接口 `interfaces.MatchedContentFetcher`，由 OpenSearch 驱动实现，并经 `KeywordsVectorHybridRetrieveEngineService` 和 `CompositeRetrieveEngine` 转发。`HybridSearch` 在截断后补读，`HybridSearchWithRerank` 在 rerank 候选池截取后补读。补读失败时只把 `MatchedContent` 留空并打 WARN |
| T7 | 部分完成 | 完成正文梯度（6.4 第一行）和 6.2 集成测试。召回数梯度、1000 库、并发、请求取消这几行未做 |
| T8 | 完成 | 见 10.3 |

### 10.3 其他后端排查（只排查，未修改）

| 后端 | 检索时是否回读向量 | 说明 |
| --- | --- | --- |
| Elasticsearch v8 | 否 | 向量和关键词两条主路径都带 `Source_` 排除 `embedding`（`v8/repository.go:504-514`、`:564-573`） |
| **Elasticsearch v7** | **是** | 关键词查询没有 `_source` 过滤，也没有 `size`（默认 10 条）（`v7/repository.go:776-791`）。文档写入时带 `embedding` 字段。`Retrieve` 只走关键词路径 |
| **Milvus** | **是（很可能）** | 两条路径都使用 `WithOutputFields("*")`，`convertResultSet` 会解码 `embedding` 列（`milvus/repository.go:787`、`:877`、`:1376-1391`）。迁移路径已改为显式字段列表 |
| Qdrant | 否 | 未设置 `WithVectors`（默认不返回） |
| Postgres/pgvector | 否 | SELECT 列表不含 `embedding` |
| SQLite | 否 | 显式列出字段 |
| Weaviate | 否 | `_additional` 只取 id 和分数 |
| Tencent VectorDB | 否 | `RetrieveVector: false` |
| Doris | 否 | `columnsForRetrieve` 不含 `embedding` |

ES v7 和 Milvus 建议另开缺陷单。各后端均没有应用层的响应字节上限，只有 OpenSearch 有。

### 10.4 集成与负载测试数据（本机 WSL OpenSearch 2.19.3，单分片，4096 维）

测试为 `integration/local_real_bf02_test.go`，用 `-tags integration` 运行，`PRIMIND_OS_LOCAL_URL=http://127.0.0.1:9200`。客户端上限保持默认 16 MiB，候选数 `TopK=500`。

**6.2：7 库 × 200 片段，正文 2～40 KiB 随机，含图片行和生成问题行**

| 检索 | 开关关闭：响应字节 / 耗时 | 开关打开：响应字节 / 耗时 | 补读 10 条 | 一致性 |
| --- | --- | --- | --- | --- |
| 向量 | 8,832,429 B / 658 ms | 132,984 B / 269 ms | 17 ms | 500 条 ID、顺序、分数一致；10 条匹配内容一致 |
| 关键词 | **16,092,559 B（上限的 96%）** / 497 ms | 132,615 B / 246 ms | 21 ms | 同上 |

开关打开后，500 条候选的响应约 130 KB，满足 AC5（小于 512 KiB）。即使已做字段过滤，开关关闭时关键词查询也已逼近上限。

**R1 前缀性质（真实集群，单分片）**：客户端上限设为 3 MiB 以触发降级。向量查询经 3 次请求保留前 125 条，关键词查询经 4 次请求保留前 62 条，两者都是完整排序的前缀，分数一致。多分片场景未验证。

**6.4 正文梯度：10 库 × 50 片段，正文定长**

| 每条正文 | 检索 | 开关关闭：返回条数 / 请求次数 / 截断 / 字节 / 耗时 / 单次分配 | 开关打开：返回条数 / 字节 / 耗时 |
| --- | --- | --- | --- |
| 8 KiB | 向量 | 500 / 1 / 否 / 3.26 MB / 300 ms / 39.4 MiB | 500 / 128 KB / 217 ms |
| 8 KiB | 关键词 | 500 / 1 / 否 / 3.26 MB / 250 ms / 39.4 MiB | 500 / 128 KB / 213 ms |
| 32 KiB | 向量 | 500 / 1 / 否 / 12.6 MB / 448 ms / 156.7 MiB | 500 / 131 KB / 211 ms |
| 32 KiB | 关键词 | 500 / 1 / 否 / 12.6 MB / 382 ms / 156.7 MiB | 500 / 130 KB / 204 ms |
| 64 KiB | 向量 | 250 / 2 / **是** / 12.4 MB / 842 ms / 186.3 MiB | 500 / 131 KB / 245 ms |
| 64 KiB | 关键词 | 250 / 2 / **是** / 16.5 MB / 906 ms / 222.8 MiB | 500 / 130 KB / 238 ms |

64 KiB 正文在开关关闭时触发降级：返回前 250 条并标记截断，不再报错（AC6 的驱动部分）。字节数只统计最后一次成功的响应。

### 10.5 进程内存峰值

未测。16/32 路并发负载未做。上表中“单次分配”是一次检索期间 Go 的累计分配量（`TotalAlloc` 差值），不是常驻峰值，约为响应体积的 12～13 倍。按第 4.5 节，并发峰值需要另行实测，再对照 Pod 1 GiB 上限。

## 11. 风险与待确认

- R1：向量查询保持 `k`、只减小 `size` 的前缀性质，需要在 OpenSearch 2.19.3 的多分片场景下验证。如果不成立，降级后的结果只能保证"头部近似一致"，需要在 AC4 中注明。
- R2：方案 B 会多一次网络往返，但只读取约 `MatchCount` 条，预计在 20 ms 以内。在第 6.2 节测试中记录实际增幅。
- R3：方案 A 依赖"索引正文 = 标题 + 正文"这一写入规则。规则改动时，重建逻辑必须同步修改；应补充测试，把两处规则绑在一起。
- 待确认：部分结果的提示文案，需要产品确认。
- 待确认：16 MiB 这个数值没有文档依据（见第 12 节）。是否按应用 Pod 内存和并发数重新核定，以及是否需要配置入口，需要另行决定；本期保持不变。

## 12. 16 MiB 上限的来历（2026-11-25 核查）

**问题**：16 MiB 是谁定的、为什么是这个数、能不能改。

**两处限制，来源不同**

| 位置 | 引入时间与提交 | 作用 | 数值依据 |
| --- | --- | --- | --- |
| 解析层 `LimitReader`：[retrieve.go:138](../../../internal/application/repository/retriever/opensearch/retrieve.go#L138)、[bulk_update.go:113](../../../internal/application/repository/retriever/opensearch/bulk_update.go#L113)、[move.go:53](../../../internal/application/repository/retriever/opensearch/move.go#L53) | 2026-10-14 初始提交 `20aa3d2` 已存在 | 只限制解码器读取的长度。SDK 在交给适配层之前已经把整个响应读进内存，所以它限不住内存 | 无说明，只有一句注释说结果大小受"最大结果窗口 × 文档大小"约束 |
| 传输层硬上限：[runtime.go:31](../../../internal/application/repository/retriever/opensearch/runtime.go#L31) `DefaultRuntimeOptions`，超限判断在 [runtime.go:283](../../../internal/application/repository/retriever/opensearch/runtime.go#L283) | 2026-10-29 OS-01 阶段（`d3f59e9`），11-03 随 `d887558` 迁入 main | 在读取响应时就截断，超限返回 `ErrResponseTooLarge`（本缺陷的报错来源） | 无说明，看起来沿用了解析层已有的 16 MiB |

**谁要求的**：[OS-01 任务文档](../../implemented/phase-2-ai-development/tasks/OS-01-本地核查与必要修复.md)第 142 行要求：必要时在 OpenSearch 专用传输层限制实际读取量，超限报错关闭，避免巨型响应撑大内存。文档只要求"必须有界"，**没有规定数值**。

**为什么需要上限**：传输层会把整个响应读进内存（`io.ReadAll`）后才交给后续代码。进程级请求预算最多允许 16 个请求同时进行：

- 最坏情况光缓冲就是 16 × 16 MiB = 256 MiB；
- JSON 解码还要再复制一份正文，峰值约 512 MiB；
- 应用 Pod 内存上限为 1 GiB（[helm/values.yaml](../../../helm/values.yaml#L86)）。

这个上限实际防止的是并发检索把应用撑到 OOM。

**能不能改**

- 代码允许的范围是大于 0、不超过 64 MiB（[runtime.go:36](../../../internal/application/repository/retriever/opensearch/runtime.go#L36)）。
- 但生产环境的两处客户端创建点（[engine_factory.go:187](../../../internal/container/engine_factory.go#L187) 和 `container.go:1391`）都没有传入运行参数，**没有任何环境变量或配置项能改它**。OS-03 文档也要求：需要可配置时，沿现有配置体系做最小接线，不能假定已有环境变量。
- 只调高传输层上限无效：三处解析层 `LimitReader` 仍会在 16 MiB 截断，表现为 JSON 解析失败（本期 T1 修正）。
- 调高有内存代价：改成 32 MiB 时，满并发光缓冲就有 512 MiB，加上解码约超过 1 GiB。调高单个响应的上限，必须同时降低并发许可数或调高 Pod 内存。

**结论**：本期保持 16 MiB，靠字段过滤和超限降级解决问题。数值是否重新核定、是否接入配置项，列入第 11 节待确认。
