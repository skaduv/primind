# OpenSearch 二期进度记录

分支：`claude/os-phase2-9b4d6d`（从 main 切出）。验收 ID 来自 `docs/specs/phase-2-ai-development/03-opensearch.md` 第 5、6 节。
本记录只保留状态、验证方式、结果和未完成项，不另存证据文件。

## 最终 AC 状态（2026-11-03）

分支基于最新本地 main（`fc23efc`），无需 rebase。`go build ./...` 通过；opensearch（含 integration 和 `-tags integration` 真实测试）、service/retriever、container、chat_pipeline 全部通过；service 包仍只有已知的 58 个本机失败（见“已有问题”）。

| AC | 状态 | 主要提交 | 未覆盖 / 备注 |
| --- | --- | --- | --- |
| AC-OS02-01 存储与绑定 | 通过 | 03fb800、e7e786b | — |
| AC-OS02-02 标识一致、维度、重复写 | 通过 | 03fb800、e7e786b | — |
| AC-OS02-03 三路召回 | 通过 | e7e786b | — |
| AC-OS02-04 问答引用 | 通过 | e7e786b | 问答 SSE 不发 complete 是通用问题，未修（见“已有问题”） |
| AC-OS02-05 重启后数据保留 | 通过 | fd3e479 | — |
| AC-OS02-06 原后端与 Wiki 冒烟 | 通过 | ceba0ea | Wiki 自动生成受 `wikiLLMMaxTokens=32768` 影响，属已有问题，未修 |
| AC-OS02-07 性能（纯索引/持续写查） | 部分：适配器和应用级基线已测 | 621f1bb、a05c88e、72a61ac | 未做并发混合负载和资源曲线；不能关闭 |
| AC-OS03-01 修改/重解析/停用/标签/移动/复制 | 通过 | 07e8452、2c51ff3、7cbeb6e | — |
| AC-OS03-02 删除文档 | 通过（含 OpenSearch 不可用时） | 07e8452、7cbeb6e、8e7b820 | 暂停 40 秒期间保持 deleting，恢复后完成，无残留 |
| AC-OS03-03 删除知识库 | 通过（含 OpenSearch 不可用时，已修复） | 7cbeb6e、8e7b820 | 修复删向量失败仍报完成、留下孤儿索引的缺陷 |
| AC-OS03-04 权限 | 通过 | 467511e、c67791c、9ba3f7b | 9ba3f7b 修复了问答图检索路径上的跨租户越权；图路径用 mock 覆盖，没有起 Nebula |
| AC-OS03-05 bulk 部分失败 | 通过 | 47d6ca8 | — |
| AC-OS03-06 断连与对账 | 通过 | 47d6ca8、c0ff6a1、64a6669 | 64a6669 修复了摘要阶段断连后的摘要 chunk 残留；旧写重放为后写覆盖，顺序由任务层保证 |
| AC-OS03-07 registry 资源回收/冷却/替换 | 通过 | 47d6ca8 | — |
| AC-OS03-08 重启恢复与退出释放 | 通过（单测验证退出释放） | 47d6ca8、f8b527e | Windows 下无法对后台进程发 Ctrl+C，没有在真实进程上走一遍优雅退出 |
| AC-OS03-09 业务性能目标 | 未关闭 | 72a61ac | 目标值待用户确认；未做正常/突发混合负载 |

## 本机环境与运行约定

- 依赖：WSL Docker 中的 PG(ParadeDB pg17) 5432、Redis 6379、MinIO 9000、OpenSearch 2.19.3 `http://127.0.0.1:9200`、docreader 50051。
- 隔离：PG 库 `primind_os`、Redis DB 1 前缀 `os:`、MinIO bucket `primind-os`、索引前缀 `primind_os_*`；应用端口 8081，前端 5174；本机覆盖值写未跟踪的 `.env.local`，不改被跟踪的 `.env`。
- Windows 本机构建注意（仅环境，不改仓库）：
  - `sqlite-vec` 的 cgo 需要 `sqlite3.h`：在临时目录放一个 `#include "<go-sqlite3 模块目录>/sqlite3-binding.h"` 的 `sqlite3.h`，`CGO_CFLAGS` 加 `-I<该目录>`。
  - Git Bash 下运行测试/程序时须把 WinLibs mingw64 `bin` 放在 PATH 最前，否则会加载 Git 自带的 `libstdc++-6.dll`，`gojieba` 在 `internal/types` 初始化时崩溃（0xc0000005）。

## OS-A 代码迁移

状态：完成。

- 来源：`codex/phase2-os-02@a5e69ab` 的 `git diff main...codex/phase2-os-02 -- internal`（38 个文件），未搬 `outputs/`、`docs/`。main 在合并基之后没有改动 `internal/`，补丁直接应用。
- `integration/testdata/*.txt` 需按 `.gitattributes` 保持 LF，否则 `TestFrozenFixturesAndIndependentOracle` 失败。
- codex 的真实入口（`TestRealSameChunkIdempotency`/`TestRealPerformance`）依赖审批配置流程，按当前决定不再使用；新增 `integration/local_real_test.go`，直接对本机 OpenSearch 跑写读删：

```bash
go build ./...
go test ./internal/application/repository/retriever/opensearch/... ./internal/container/ -count=1
PRIMIND_OS_LOCAL_URL=http://127.0.0.1:9200 go test -tags integration ./internal/application/repository/retriever/opensearch/integration -run '^TestLocalReal' -count=1 -v
```

结果：build 通过；单测、loopback 集成测试通过；`TestLocalRealWriteRetrieveDelete`、`TestLocalRealKeywordOnlyAndCopy` 在 2.19.3 上通过（Save/BatchSave 重复写不增重复、向量维度、向量/关键词检索与 KB 过滤、停用/标签更新、按文档删除不误删、关键词索引路径、同 store KB 复制），测试结束只删除自己创建的 `primind_os_it_*` 精确索引。

## OS-B 真实闭环（AC-OS02-01~06）

状态：AC-OS02-01~06 已通过（AC-06 中 Wiki 自动生成失败，属于与 OpenSearch 无关的已有问题，见下文）。

样本与问题：沿用 `integration/testdata` 中冻结的合成样本 `OS_SAMPLE_A.txt`、`OS_SAMPLE_B.txt`，以及 `oracle.json` 里的 Q-KW/Q-VEC/Q-HYB/Q-QA/Q-NEG。请求体用 UTF-8 文件发送（Git Bash 下把中文直接放在 curl 参数里会乱码）。

模型鉴权：`POST /initialization/remote/check`（chat）和 `POST /initialization/embedding/test` 只带 `modelId`，由服务端补全已保存的 Key，返回“连接正常”和“向量维度=4096”。应用重启后复核结果相同。

| AC | 结果 |
| --- | --- |
| AC-OS02-01 存储与绑定 | 用户 A 的 OpenSearch store `8896d60e…`（`http://127.0.0.1:9200`，index_name `primind_os_app`）绑定新知识库 `60e8a63c…`（Embedding `builtin-company-embedding`，向量+关键词）。上传样本后自动建索引 `primind_os_app_8896d60e-a60_4096_v1`（alias `…_4096`），并有真实写入和读取。 |
| AC-OS02-02 标识一致、维度、重复写 | 解析完成后，数据库 `chunks` 与 OpenSearch 文档 `_id` 逐条一致：每个样本 1 个 text + 1 个 summary，共 4 条，knowledge_base_id 一致，is_enabled=true。存储的向量长度 4096。Postgres `embeddings` 表中该知识库为 0 条，数据只在 OpenSearch。对样本 A `POST /knowledge/:id/reparse`：生成新 chunk ID，OpenSearch 与数据库仍逐条一致，旧 ID 全部消失，没有重复。 |
| AC-OS02-03 三路召回 | `POST /knowledge-bases/:id/hybrid-search`。Q-KW（关闭向量）：只命中 A 的 text 和 summary，match_type=关键词。Q-VEC（关闭关键词）：A 的两条排在前面（0.749/0.712），B 的两条在后（0.459/0.418），作为补充命中。Q-HYB：A 的 text 排第 1，结果中没有重复 chunk ID，沿用现有融合逻辑。所有结果都只来自本知识库。 |
| AC-OS02-04 问答引用 | 新会话 `POST /knowledge-chat/:session_id`，限定本知识库，问答模型 `builtin-company-chat`。Q-QA 的回答给出 SEAL_A_101 和 WO_A_001，注明来源 `OS_SAMPLE_A.txt`，正文引用 `chunk_id=2d3f12d8…`（A 的 text chunk，存在于 OpenSearch）；references 中是 A 的 text/summary 和 B 的补充片段。Q-NEG 回答资料未提及 COMP_C 及其部件，没有把 A/B 的部件安到 COMP_C 上。回答原文以数据库 `messages` 表为准。 |

AC-OS02-05 重启 OpenSearch 后数据保留：通过。

- 操作：只重启 `primind-dev-opensearch` 容器（WSL 中执行 `docker restart`），数据卷为 `primind-dev_opensearch-data -> /usr/share/opensearch/data`；没有重启应用。
- 重启前：集群 yellow、1 节点（单节点、副本未分配，见“零副本”风险项）；索引 `primind_os_app_8896d60e-a60_4096_v1` 共 4 条。
- 重启：约 16 秒后可连接，状态先是 red（分片恢复中），再过约 2 秒变为 yellow。
- 重启后：同一索引仍是 4 条，该知识库的 4 个 chunk `_id` 不变；应用端 Q-KW/Q-VEC/Q-HYB 返回的 chunk ID 和顺序与重启前完全一致（逐行 diff 相同）。应用不需要重启就恢复检索。

问答流的问题（未定位）：Q-QA 两次请求，服务端日志都在约 21 秒内显示 `Knowledge QA service completed`、`channel_close`，但 SSE 没有发出 `complete` 事件，连接一直不关闭，直到客户端超时（590 秒）。Q-NEG 那次正常收到 `complete`。数据库中消息是完整的（`is_completed=t`）。对照：同一 Q-QA 分别针对原 Postgres 后端知识库和 OpenSearch 知识库各跑一次，两边都收到 33 个 answer 事件、0 个 complete，120 秒后由客户端断开。所以这是问答 SSE 的通用问题，与 OpenSearch 无关，不在本分支修复，另行跟进。

AC-OS02-06 原后端与 Wiki 冒烟：通过（入口和检索）；Wiki 自动生成失败，属于已有的通用问题。

- 原后端：知识库 `825e42be…`（未绑定 store，即 Postgres 后端，`RETRIEVE_DRIVER=postgres`）上传样本 A 后解析完成；Postgres `embeddings` 中 2 条，OpenSearch 中该知识库 0 条；hybrid-search 命中样本 A 的 text/summary。问答同样可用，SSE 表现见上一段。
- Wiki 页面入口：wiki 知识库 `ef9a9db2…`（Postgres 后端，`wiki_config.synthesis_model_id=builtin-company-chat`）上，`POST /knowledgebase/:kb_id/wiki/pages` 返回 201；`GET pages`、`GET pages/:slug`、`GET search?q=PUMP_A`、`GET index`、`GET stats` 均返回 200，能读回新建页面。
- Wiki 自动生成（未通过，与 OpenSearch 无关）：上传文档后 wiki ingest 任务确实执行，但每次调用 LLM 都返回 400：“maximum context length is 32768 tokens. However, you requested 32768 output tokens”。原因是 `internal/application/service/wiki_ingest.go:192` 写死了 `wikiLLMMaxTokens = 32768`，没有按模型上下文窗口收敛；本机问答模型只有 32k 上下文，所以 wiki 抽取必然失败，重试 5 次后放弃，页面数为 0。这在 main 上就存在，本分支不改，另行跟进（可选：按模型 `context_window` 限制 MaxTokens，或在本机模型配置上调整）。

准备情况（均为未跟踪的本机文件，不提交）：

- 模型：`config/builtin_models.yaml`（由 `.git/info/exclude` 忽略），指向公司内网 172.16.16.120 上的服务：问答 47501、视觉 47502、Embedding 47503（Qwen3-Embedding-8B，4096 维）、重排 47504（本机直连超时，知识库不选用）。Key 通过 `${PRIMIND_LOCAL_*_API_KEY}` 从 `.env.local` 读取，加载器 `LoadBuiltinModelsConfig` 会自行展开；`AUTO_MIGRATE=true`（来自 `.env`）。
- `.env.local` 追加了 4 个空的 Key 占位，`NO_PROXY`/`no_proxy` 包含 `172.16.16.120`（本机 HTTP(S)_PROXY 走 7890 会返回 502）。
- SSRF 白名单：本库的测试用户不是系统管理员，系统设置 API 返回 403。我没有通过 SQL 提权，改为在 `.env.local` 设置 `SSRF_WHITELIST_EXTRA=127.0.0.1,172.16.16.120`；应用启动时会把它与系统设置里的白名单合并（`applySSRFWhitelist`），原有条目保留（当前系统设置为空）。

4096 维核对：

- 适配器允许的维度为 1..16000；`TestLocalRealBaseline` 以 dim=4096、lucene HNSW 实测 2000 条：写入约 268 chunk/s，向量检索 p95 78.2 ms，关键词检索 p95 27.9 ms，计数一致。
- 导入路径没有问题：索引器每批 40 条，约 1.8 MB，低于 bulk 10 MB 上限。
- **问题：知识库复制在 4096 维下会失败。** `CopyIndices` 每扫一页 500 条，就用一次 BatchSave 写入；4096 维估算单条约 21.6 KB（真实 JSON 约 45 KB），超过 10 MB 上限，返回 `ErrBatchTooLarge`。真实集群临时复现：复制 320 条全部未发送（`320 not sent`），目标为 0 条，错误会明确返回，不会静默。按当前上限，大约超过 220 条的知识库就无法复制。**已修复**，见 OS-C“4096 维知识库复制”。
- SSRF：系统设置白名单这条路径没有验证，原因是测试用户不是系统管理员（不通过 SQL 提权）；本机改用 `SSRF_WHITELIST_EXTRA`。

### 新旧后端问答对比、SSE 复测与切回 MinIO（2026-11-04，时钟已修正）

文件存储切回 MinIO：`.env.local` 改为 `STORAGE_TYPE=minio`（bucket `primind-os`）。说明：存储按 storage backend 绑定，租户默认值仍指向之前的 `System LOCAL`，所以只改环境变量时，已有知识库上传的文件仍会写到本地；通过 `PUT /storage-backends/14c112d3…/default` 把测试租户的默认值改为 `System MINIO` 后，新建的知识库就绑定 MinIO。验证：
- `POST /storage-backends/:id/test` 成功；新知识库上传后 `knowledges.file_path` 为 `resource://…`，presigned-preview 返回 `provider=minio`，URL 指向 `127.0.0.1:9000/primind-os/…`，`X-Amz-Expires=86400`。
- `GET /knowledge/:id/download` 返回 200，内容与原文件逐字节一致；直接 GET 预签名 URL 返回 200，内容一致；篡改签名后返回 403。

问答对比：新建两个知识库，内容相同，都上传 `OS_SAMPLE_A.txt`、`OS_SAMPLE_B.txt`，Embedding 用 `builtin-company-embedding`（4096 维），问答模型 `builtin-company-chat`，不用重排。`cmp-pg` 为 Postgres 后端，`cmp-os` 绑定 OpenSearch store `8896d60e…`。每题各在新会话中问一次。“检索耗时”是同一问题调用 `hybrid-search`（TopK 5）的应用端总耗时；“问答耗时”是 SSE 从请求到结束的时间，主要花在 LLM 上。

| 题目 | 预期 | PG 回答 / 引用 | OS 回答 / 引用 | PG 检索 / 问答 | OS 检索 / 问答 |
| --- | --- | --- | --- | --- | --- |
| Q1 唯一检索标记 OSFIXA20261022 对应哪台设备？ | 泵 PUMP_A | 正确 / A text | 正确 / A text | 0.23 s / 15 s | 0.19 s / 19 s |
| Q2 泵 PUMP_A 轴封渗漏时工单要求更换什么？ | SEAL_A_101 | 正确 / A text + A summary | 正确 / A text + A summary | 0.16 s / 31 s | 0.23 s / 30 s |
| Q3 OSFIXA20261022，WO_A_001 要更换的部件号？ | SEAL_A_101 | 正确 / 引用原文（无 chunk 标签） | 正确 / 引用 A 原文（无 chunk 标签） | 0.22 s / 23 s | 0.16 s / 29 s |
| Q4 风机 FAN_B 轴承异响换哪个部件、工单号？ | BEARING_B_202、WO_B_002 | 正确 / B text + B summary | 正确 / B text | 0.23 s / 26 s | 0.45 s / 30 s |
| Q5 资料是否给出压缩机 COMP_C 的齿轮部件号？ | 未给出 | 正确拒答 / 无 | 正确拒答 / 无 | 0.26 s / 18 s | 0.23 s / 27 s |

结论：两种后端 5 题全部答对，引用都落在正确的样本上；hybrid-search 的首个命中 PG 与 OS 一致（Q4 为 B，其余为 A）。检索耗时同一量级（PG 0.16–0.26 s，OS 0.16–0.45 s，含 4096 维查询向量化的网络调用）。问答总耗时主要取决于 LLM，没有系统性差异。样本只有两篇，两篇都会出现在 references 中，所以引用正确性以回答中 `<kb chunk_id=…>` 实际引用的 chunk 为准。

SSE 复测：时钟修正后 10 次问答全部收到 `complete`，连接正常关闭。此前的“不发 complete”确认是时钟跳变引起，现已不再出现。

## OS-C 生命周期（AC-OS03-01~03）

状态：适配器层和应用级均已在真实环境验证，修复了两个缺陷（按查询操作看不到刚写入的数据、4096 维复制）；仅“删除任务失败不被当作完成”未覆盖。

已修复缺陷：按查询执行的操作看不到刚写入的数据。

- 现象（真实集群复现）：`BatchSave` 后立即执行以下操作，它们只读已 refresh 的快照，而 bulk 写入不带 refresh。结果是：`DeleteByKnowledgeIDList` 留下全部 3 条残留；`BatchUpdateChunkEnabledStatus` 停用无效，片段仍可检索；`CopyIndices` 只复制了 0/3；`MoveKnowledgeIndices` 返回成功但 0 条被移动。服务层的索引失败清理、写入后检测到删除时的清理、停用和标签更新都是这种紧跟写入的调用。
- 修复：新增 `refreshBeforeByQuery`，在 delete_by_query、update_by_query（停用/标签）、移动、复制扫描前，先 refresh 对应的本 store 索引（`<base>_*` 或当前维度 alias）。没有改变 bulk 写入的刷新策略，也没有改刷新间隔。
- 性能代价：每次 by-query 操作前多一次 refresh 请求，并强制生成新段。本机在 40 条新写入后测得：单独 refresh p50 9.9 ms、p95 68.1 ms；停用 1 个 chunk 的整个调用（refresh + update_by_query + 自带 refresh）p50 27.7 ms、p95 119.3 ms。这些操作都是低频的管理/清理调用，不在导入和检索热路径上。批量删除或停用时，每次调用只 refresh 一次，不随 chunk 数增加。如果高频调用（例如大量逐条停用）产生过多小段，可以考虑改为调用方合并请求。详见 OS-F。
- 测试：`TestLocalRealByQueryOpsSeeFreshWrites` 与 `TestLocalRealMoveWithinStore` 修复前失败、修复后通过。单测替身改为把 `_refresh` 当作非数据写入，并断言 refresh 先于移动执行。

适配器层已覆盖（真实集群，见 OS-A 命令）：停用片段不再被检索、标签更新、删除文档不误删其他知识库、同 store 移动（向量和关键词索引都改为目标知识库并清空标签，源知识库不再命中，其余文档保留）、同 store 复制（源保留、目标 ID 重映射、向量完整）。

已修复缺陷：4096 维知识库复制失败（AC-OS03-01）。

- 现象：`CopyIndices` 每扫一页 500 条，就用一次 BatchSave 写入。4096 维时一次 bulk 超过 10 MB 上限，返回 `ErrBatchTooLarge`；拆分写入后，500 条的扫描响应又超过客户端 16 MB 的响应上限，返回 `ErrResponseTooLarge`。真实复现：复制 320 条，目标 0 条。
- 修复（只改适配器）：
  - 新增 `splitForBulk`，按 BatchSave 的三项上限切分写入批：条数 `maxBulkItems`、预估字节 `estimateBulkBodyBytes`、实际序列化字节 `maxBulkBytes`。字节数用与发送时相同的投影和 `jsonSize` 精确计算，不写死条数。
  - 扫描页大小 `copyScanPageSize` 按维度和该客户端实际的响应上限计算（768 维仍是 500，4096 维为 120）。遇到 `ErrResponseTooLarge`（例如正文特别长）时，同一偏移量的页大小减半后重试。
  - 部分写入失败时，报错带上已复制的条数。
- 测试：`TestLocalRealCopyLargeDim`（真实集群，4096 维，620 条，跨扫描页）：源、目标各 620 条，抽查首尾和页边界的 chunk，正文、启用状态、knowledge_id 映射和 4096 维向量都与源一致。单测 `TestSplitForBulk_EveryGroupFitsBatchSaveLimits`（500×4096、500×768、1500 条无向量、16000 维）确认每组都能通过 `buildBulkBody`，并且顺序和覆盖完整；`TestCopyScanPageSize_FollowsDimensionAndCap` 覆盖页大小计算。

应用级复核（真实应用 8081 + 公司 Embedding 4096 维，绑定 OpenSearch store `8896d60e…`）：每一步都用 SQL 查 `chunks` 中未删除的 ID，并与 OpenSearch 中按 knowledge_id 或 knowledge_base_id 搜到的 `_id` 逐条比对（先 refresh）。以下各项均通过：

| 场景 | 操作 | 结果 |
| --- | --- | --- |
| 修改 | `PUT /knowledge/manual/:id`，第一版标记 ALPHAOSC111，改为第二版 BETAOSC222 | 重新生成 chunk，数据库与 OpenSearch 一致；检索旧标记 0 条，新标记命中 |
| 重解析 | `POST /knowledge/:id/reparse`（样本 A） | 见 AC-OS02-02：新 ID 与数据库一致，旧 ID 全部消失 |
| 停用/启用 | `PUT /chunks/:knowledge_id/:id {"is_enabled":false/true}` | 停用后服务层从索引删除该 chunk（GET 返回 `found:false`），检索不再返回；启用后重新写入（`is_enabled:true`），检索恢复 |
| 标签范围 | `PUT /knowledge/tags` 给样本 A 打标签，问答请求带 `tag_ids` | 打标签后，即使问 B 的标记，引用也只来自 A；清除标签后，同一范围的问答引用为空。注意：`hybrid-search` 的 `scope_tag_ids` 只用于追踪，文档标签的过滤在问答链路中先解析成 knowledge_ids 再下发给 OpenSearch；`tag_ids` 在 hybrid-search 中是 FAQ chunk 标签的物理过滤。Postgres 知识库表现相同 |
| 同 store 移动 | `POST /knowledge/move`（reuse_vectors），手工文档移到同 store 的第二个知识库 `d6267cca…` | chunk ID 保持不变；OpenSearch 中 knowledge_base_id 改为目标知识库；源知识库检索不再命中，目标命中；源知识库其他文档保留（数据库 4 条 = OpenSearch 4 条） |
| 知识库复制 | `POST /knowledge-bases/copy`，任务完成 | 目标知识库绑定同一 store 和同一 Embedding；源、目标各 4 条，数据库 = OpenSearch；复制后的向量与源逐字节相同（摘要一致）；源知识库仍可检索 |
| 删除文档 | `DELETE /knowledge/:id`（复制出的知识库中的样本 B，异步任务） | 该文档的 2 个 chunk 在 OpenSearch 中 `found:false`，数据库 0 条；同一知识库的另一文档和源知识库中的样本 B 不受影响 |
| 删除知识库 | `DELETE /knowledge-bases/:id`（复制出的知识库） | 该知识库在 OpenSearch 中 0 条，原有 chunk ID 均 `found:false`，数据库 0 条，GET 返回 not found；共用的物理索引保留，其他两个 OS 知识库的计数不变，A/B 仍可检索。没有任何通配删除 |

未覆盖：“异步删除任务失败时不被当作完成”需要在删除过程中让 OpenSearch 不可用（停止容器）。当前只获授权重启容器，这一项没有做；适配器层的删除响应完整性校验见 `TestOS01_DeleteRequiresCompletion`。

另记：`MoveKnowledgeIndices`、停用/标签更新、无维度关键词检索使用 `<base>_*` 通配。env 路径的 base 是裸 `primind`，可能匹配到同集群其他 store 的索引；因为过滤条件是唯一的 chunk/KB ID，实际不会串数据，暂不处理。

## OS-D 权限（AC-OS03-04）

状态：通过。ID 越权、共享和撤销在元数据层和 OpenSearch 检索内容层都已验证。

环境：应用 8081，两个独立租户 A（10000）、B（10001），测试账号存于未跟踪的 `.local-run/test-accounts.env`。A 建 OpenSearch store `8896d60e…` 并绑定关键词知识库 `ad7f1580…`，文档 `1e2168fe…`（因无 Embedding 模型解析失败，元数据仍在）。B 直接带 A 的 KB/文档 ID 请求：

| 请求 | B 越权 | A 共享给组织（viewer）后 B | 撤销共享后 B |
| --- | --- | --- | --- |
| `GET /knowledge-bases/:id` | 403 | 200 | 403 |
| `GET /knowledge-bases/:id/knowledge` | 403 | 200 | 403 |
| `GET /knowledge/:id` | 403 | 200 | 403 |
| `GET /chunks/:knowledge_id` | 403 | 200 | 403 |
| `POST /knowledge-bases/:id/hybrid-search` | 403 | 200 | 403 |
| `GET /knowledge/search?kb_id=` | 200，结果为空（按调用者租户过滤） | 200 | 200，结果为空 |
| `POST /knowledge-bases/:id/knowledge/manual` | 403 | 403（viewer 不能写） | 403 |
| `DELETE /knowledge/:id` | 403 | 403 | 403 |

共享流程：A `POST /organizations` → B `POST /organizations/join`（邀请码）→ A `POST /knowledge-bases/:id/shares`（viewer）→ A `DELETE /knowledge-bases/:id/shares/:share_id`。撤销后同样的请求立即失效。

内容级复核（有 Embedding 后，在已入索引的 OpenSearch 知识库 `60e8a63c…` 上进行，B 发送 Q-HYB）：

- 共享前：B 对该知识库和同 store 的另一知识库 `d6267cca…` 调用 hybrid-search，都返回 403，没有内容。
- 共享（viewer）后：B 对 `60e8a63c…` 返回 200，命中只来自该知识库（knowledge_base_id 只有这一个，文档为样本 A/B）；对未共享的 `d6267cca…` 仍是 403。
- 绕过尝试：B 在共享期间请求 `/knowledge-bases/60e8a63c…/hybrid-search`，在 body 里用 `knowledge_base_ids` 覆盖为 `[d6267cca…]` 或 `[60e8a63c…, d6267cca…]`，都返回 404 “knowledge base not found”，拿不到未授权知识库的内容。
- 撤销后：同样的请求立即返回 403。

## OS-E 故障恢复（AC-OS03-05~08）

状态：不依赖模型的部分已完成；索引任务期间断连、真实重放与 OpenSearch 容器重启待模型/用户操作。

验证命令（真实集成测试同 OS-A，新增 `local_real_fault_test.go`）：

```bash
PRIMIND_OS_LOCAL_URL=http://127.0.0.1:9200 go test -tags integration ./internal/application/repository/retriever/opensearch/integration -run '^TestLocalReal' -count=1 -v
go test ./internal/application/service/retriever ./internal/application/repository/retriever/opensearch/... ./internal/container -count=1
```

| AC | 状态 | 方式与结果 |
| --- | --- | --- |
| AC-OS03-05 bulk 部分失败 | 通过（适配器层，真实集群） | `TestLocalRealBulkPartialFailure`：一条 tag_id 超过 Lucene 32766 字节，真实返回 HTTP 200 + errors=true；`BulkWriteError` 为 2 confirmed / 1 failed / 0 unknown，失败位置=1，类型 `illegal_argument_exception`，索引实际 2 条。注：整请求级 400（如 `_id` 超 512 字节）会被记为全部 unknown，结果偏保守但不误报成功。服务层遇到错误会删除该文档的 chunk 与索引并按重试/失败处理（`knowledge_process.go` BatchIndex 失败分支）。 |
| AC-OS03-06 断连与对账 | 适配器层通过；任务期间断连待做 | `TestLocalRealConnectionLossAndReconcile`：经可断开的 TCP 代理写入，断开后 BatchSave 返回 `BulkWriteError`（4 unknown，`errors.Is(ErrTransport)`），检索也报错；恢复后重写同批，数量 4、内容逐条一致、无重复。`TestLocalRealStaleReplayIsLastWriterWins` 记录现状：适配器按 `_id=chunk_id` 后写覆盖，无版本守卫，延迟重放会覆盖新内容、复活已删 chunk，顺序须由任务层保证。 |
| AC-OS03-07 registry 资源回收 | 已修复并通过单测 | 缺陷：注销、同 store 替换、并发构建落败时，被丢弃的 OpenSearch 引擎从不释放连接池（registry 原注释已承认）。最小修复：`opensearch.WithOwnedClient()` + `Repository.Close()` 只在拥有 client 时释放空闲连接（engine_factory 创建的 client 标记为拥有；env 路径是借用者，不关闭）；`KeywordsVectorHybridRetrieveEngineService.Close()` 转发给实现了 `io.Closer` 的仓库；`registry.go` 在注销/替换/落败构建时调用。在途请求保留各自连接，受 operation deadline 约束，归还后的连接随 idle 超时关闭。测试：`close_test.go`（拥有者关闭、借用者不关）、`registry_close_test.go`（注销、替换只关旧、同引擎重复注册不关、落败构建被关闭、非 Closer 不受影响）。其他检索后端仓库都没有 `Close()`，行为不变。冷却/重建沿用已有 `TestGetOrLoadByStoreID_*`，全部通过。 |
| AC-OS03-08 应用重启恢复 | 注册恢复已验证；优雅退出释放资源已实现（单测验证） | 真实应用：用户 A 建 OpenSearch store（`8896d60e…`，`POST /vector-stores/:id/test` 成功），重启后日志 `Loading 1 vector store(s)` → `Registered DB vector store … engine=opensearch`。优雅退出：`NewEngineFactory` 新增 `ResourceCleaner` 参数（dig 自动注入，container.go 未改），注册名为 `RetrieveEngines` 的清理项；DB store 的引擎（启动加载和按需重建都走 `createEngineServiceFromStore`）中拥有资源的会被记录，退出时 HTTP 服务排空之后统一关闭；registry 已退役的引擎再关一次是无害的重复。env 路径是借用者，不登记。测试 `TestEngineFactoryShutdownClosesOwnedOpenSearchPools`：经工厂建引擎后执行 cleaner.Cleanup，连接数回到 0；新签名下应用重启正常，两个 OpenSearch store 都重新注册。未覆盖：Windows 下无法向后台进程发送 Ctrl+C，没有在真实进程上走一遍优雅退出。 |

AC-OS03-06 后半（索引任务进行中断连，真实应用）：通过。反馈明确，恢复后数据库与 OpenSearch 数量一致；过程中发现的摘要 chunk 残留缺陷已修复（见下）。

- 环境：在 9200 前放一个独立的 TCP 代理（127.0.0.1:19201，临时程序，不入库），新建 store `1be72f24…` 指向代理（index_name `primind_os_proxy`），绑定知识库 `1e0f1b0f…`（4096 维）。共享的 OpenSearch 容器没有动。
- 操作：连续上传 8 篇文档（每篇 3 个正文 chunk），上传后立即杀掉代理，断开约 67 秒后重启代理。
- 断连期间：5 篇已写完正文、在生成摘要（finalizing），3 篇处于 processing。失败都有明确记录：摘要 span 失败并带 `bulk outcome (0 confirmed, 0 failed, 1 unknown)`；3 篇的 embedding span 为 `batch index failed`（错误码 VectorStoreWriteFailed）。失败清理时的删除也报错 `refresh before by-query: transport error`。没有任何一篇被标成 completed。
- 恢复后：约 72 秒内 8 篇全部 completed（asynq 重试）。索引中没有重复 chunk_id；正文 24 个 chunk 与数据库逐条一致；断连时处于 processing 的 3 篇完全对账一致。
- **新缺陷：** 断连时正在生成摘要的 5 篇，数据库各多出 1 条 summary chunk（数据库 37 条，OpenSearch 32 条）。原因是 `knowledge_process.go` 摘要任务（约 1697–1740 行）先 `CreateChunks` 写入摘要 chunk，再 `BatchIndex`；索引失败时直接返回错误，没有删除刚写入的数据库行；重试又用新的 UUID 生成一条摘要 chunk。所以每次在摘要阶段索引失败，都会留下 1 条没有进索引的摘要 chunk。检索走 OpenSearch，不会命中它，但 chunk 列表和按数据库读取片段的地方能看到，计数也对不上。这段是通用服务层代码，所有检索后端都会遇到。最小修复建议：`BatchIndex` 失败时删除这条摘要 chunk 的数据库行，并尽力删除其索引（outcome 为 unknown 时可能已写入）；或者重试前先清理该 knowledge 上没有进索引的旧摘要 chunk。待决定是否在本分支修。
- **已修复：** 新增 `storeSummaryChunk`。同一文档如果已有摘要 chunk，就复用它并更新内容，多出来的删掉（数据库行和索引都删），保证最后只有一条；本次新建的摘要 chunk 如果索引失败，就删除它的数据库行，并尽力删除索引（bulk 结果为 unknown 时可能已写入）。清理失败只记日志，返回的仍是原来的索引错误。单测 `knowledge_summary_store_test.go` 覆盖：索引失败后数据库没有摘要行；重试后只有一条；复用已有摘要并清掉多余的；复用的行在索引失败时保留。真实复现（同一代理、同样上传 8 篇后断连约 40 秒）：断连期间 5 个摘要 span 失败，数据库摘要行为 0；恢复后约 78 秒全部完成，8 篇逐条对账一致，数据库 32 条 = OpenSearch 32 条（24 text + 8 summary），修复前是 37 对 32。

AC-OS03-02/03 补充（OpenSearch 不可用时删除任务的失败处理）：已测，发现并修复 1 个缺陷。

- 方式：用户放行后，单独执行 `wsl.exe -e docker pause primind-dev-opensearch` 和 `wsl.exe -e docker unpause primind-dev-opensearch`，每次暂停不超过 60 秒；恢复后核对 9200 健康状态（yellow）和对照组数量。对照组是 OpenSearch 知识库 `60e8a63c…`，3 轮前后都是数据库 4 条 = OpenSearch 4 条。
- 第 1 轮（暂停约 3 秒，短于 30 秒请求超时）：删文档后状态停在 `deleting`，没有显示成功；删知识库接口立即返回成功（软删除，异步清理）。恢复后两个任务都完成，残留归零。说明短暂断连时请求只是等待，不会出错。
- 第 2 轮（暂停约 40 秒）：删文档任务在暂停期间保持 `deleting`，恢复后完成，OpenSearch 中 0 条残留，通过。**删知识库发现缺陷**：`ProcessKBDelete`（`knowledgebase.go`）删向量超时（`refresh before by-query … context deadline exceeded`）后只记 Warning，继续删除数据库 chunk 和 knowledge 行，并记录 “KB delete task completed successfully”。结果 OpenSearch 中留下 4 条孤儿记录，而能用来重试定位它们的数据库行已经删掉，再也不会被清理。这正是“异步任务失败不能被当作删除完成”。
- 修复（最小改动，`knowledgebase.go`）：删向量失败时返回错误，交给 asynq 重试，并且在删除 chunk 和 knowledge 行之前就返回，保证重试还能找到这些向量。单测 `TestProcessKBDeleteVectorDeleteFailureRetries`：去掉修复时失败（任务返回 nil），加上修复后通过，并断言 knowledge 行未被删除。
- 第 3 轮（修复后，暂停约 40 秒，删一个 KB 副本）：暂停期间 knowledge 行 2 条、chunk 4 条都保留；恢复约 14 秒后重试成功，OpenSearch、knowledge 行和 chunk 都归零，对照组 4/4 不变。
- 第 2 轮遗留的 4 条孤儿记录是本测试自己产生的，已按 `knowledge_base_id` 精确条件从该 store 的索引中删除（`deleted: 4`），没有用通配。
- 重试用尽后的最终状态（读代码确认，未修）：`DeleteKnowledgeBase` 在入队清理任务之前就把 KB 行软删除，并立即返回 “Knowledge base deleted successfully”，界面上 KB 马上消失。清理任务 `TypeKBDelete` 设置 `MaxRetry(3)`；OpenSearch 一直不可用时，按 8e7b820 的修复，每次都返回错误，共 4 次尝试后被 asynq 归档（archived）。最终状态：KB 行已软删除，knowledge 行、chunk 和 OpenSearch 中的向量都还在，只有 ERROR 日志（`KB delete task deferred`）和 asynq 里的归档任务；用户看不到任何失败状态，也没有定期扫描会去重新清理（housekeeping 只处理文档的处理状态，`task_inspector` 明确不取消 KB 删除任务）。从用户角度看是**静默卡住**：数据不会被误标为已清理，但残留只能靠运维在 asynq 中手动重跑归档任务。删除单个文档则不同，失败期间 knowledge 保持 `deleting`，用户能看到。待决定：可以给 KB 增加“删除失败”状态，或者为软删除的 KB 加残留扫描，或者在最后一次重试失败时写审计告警。

待办/风险：

- `knowledge_process.go` 的 BatchIndex 失败分支按 knowledge ID 删除 chunk 和索引；如果旧 attempt 的失败晚于新 attempt 写入，可能误删新数据（对所有后端都成立，不限 OpenSearch）。需要用模型跑真实重解析交错来复现，确认后再给最小方案。
- 显式 `number_of_replicas: 0` 因 `omitempty` 和 `config.go` 默认值投影不会生效（spec 候选缺陷），单节点上新索引副本未分配（yellow），不影响读写；是否修复待定。

## OS-F 性能（AC-OS02-07、AC-OS03-09）

状态：适配器层基线和真实 Embedding（4096 维）的应用级基线已测；业务目标值待用户确认，AC-OS02-07 和 AC-OS03-09 未关闭。

```bash
PRIMIND_OS_LOCAL_URL=http://127.0.0.1:9200 PRIMIND_OS_LOCAL_PERF=1 go test -tags integration ./internal/application/repository/retriever/opensearch/integration -run '^TestLocalRealBaseline$' -count=1 -v -timeout 20m
```

2026-11-03 本机结果：单节点 OpenSearch 2.19.3（WSL Docker），lucene HNSW m=16，1 分片 0 副本，单 worker 顺序 BatchSave，不含 Embedding 时间，没有并发业务负载（NG 会话共用同一台机器）。

| 项 | 规模/参数 | 结果 |
| --- | --- | --- |
| 写入 | 10,000 chunk，dim 768，batch 40 | 13.0 s，约 768 chunk/s；单批 p50 40.8 ms、p95 86.8 ms、max 444.9 ms；写后计数 10,000，与写入一致 |
| 向量检索 | 200 次，TopK 10，按 KB 过滤 | p50 55.4 ms、p95 63.2 ms、p99 77.5 ms、max 110.9 ms |
| 关键词检索 | 200 次，TopK 10，按 KB 过滤 | p50 9.1 ms、p95 15.2 ms、p99 20.1 ms、max 36.5 ms |
| by-query 前 refresh（OS-C 修复的代价） | 每轮先写 40 条（dim 768）再操作，50 轮 | 单独 refresh：p50 9.9 ms、p95 68.1 ms、max 82 ms；停用 1 个 chunk 全程：p50 27.7 ms、p95 119.3 ms、max 144.3 ms |

加入 refresh 测量后重跑了一次，写入 12.05 s（约 830 chunk/s），向量检索 p95 56.4 ms，关键词检索 p95 8.4 ms，和首轮在同一量级。

应用级基线（2026-11-03，真实应用 8081，公司 Embedding Qwen3-Embedding-8B 4096 维 + 问答模型做摘要，OpenSearch 单节点，导入为默认并发，没有其他业务负载；与 NG 会话共用本机）：

| 项 | 规模 | 结果 |
| --- | --- | --- |
| 导入总耗时 | 20 篇手工文档（每篇 12 段，约 1.6k 字），连续提交 | 共 80 个 chunk（60 text + 20 summary），从第一篇创建到最后一篇完成 85.7 s，失败 0；数据库与 OpenSearch 都是 80 条 |
| 分段（每篇，来自 `knowledge_processing_spans`） | 20 篇 | chunking p50 11 ms / p95 17 ms；embedding+写 OpenSearch p50 1368 ms / p95 2148 ms（每篇 3 个 text chunk）；主流程 knowledge_processing p50 2126 ms / p95 2697 ms；摘要（LLM）p50 51.9 s / p95 58.1 s，是总耗时的主要来源 |
| 应用检索延迟（`hybrid-search`，TopK 10，30 次，含 HTTP、鉴权、查询向量化、OpenSearch 和融合） | 80 chunk 的知识库 | 关键词 p50 141 ms / p95 212 ms；向量 p50 216 ms / p95 238 ms；混合 p50 227 ms / p95 252 ms；全部成功 |

对比：适配器层在 4096 维下纯写入约 268 chunk/s，向量检索 p95 78 ms；应用级的延迟差主要来自查询向量化的网络调用和服务层开销。导入的瓶颈是模型调用（Embedding 和摘要），不是 OpenSearch。

仍待定：业务目标值（规模、导入窗口或最低吞吐、p95、错误率、资源和队列上限）需要用户确认；并发写查混合、有限突发和资源曲线没有做，所以 AC-OS02-07 和 AC-OS03-09 都不能关闭。为避免压到公司共用的模型服务，本次只做了小规模测量。

这只是纯索引和检索子路径的基线，不能关闭 AC-OS02-07 或 AC-OS03-09。待做：应用级导入（解析、Embedding、索引确认、可检索）和三路检索、问答的分段耗时，并发写查混合，以及向用户确认业务目标值（规模、导入窗口或最低吞吐、p95、错误率、资源上限）。

## 与 NebulaGraph 会话的共享文件

- `internal/application/service/knowledgebase.go`：暂未改动。

## 已有问题（与本分支改动无关）

`go test ./internal/application/service` 在本机 Windows 上有 58 个失败。在不含本分支未提交改动的干净 HEAD 副本上，失败的正好是同一批 58 个：

- 沙箱查询模板 `TestQueryTemplates*`（15 个）
- 沙箱配置更新/删除时的快照与封锁 `TestUpdate*`、`TestDelete*`（沙箱/技能快照相关，约 25 个）
- SQLite 数据源与调度 `TestDataSource*`、`TestDeleteKnowledgeBaseCleansUpSQLiteDataSources`（9 个）
- 技能 Python 校验与本地执行 `TestSkillPythonVerifier*`、`TestSeededSkillHelperIsDirectlyExecutable`、`TestRuntimePrerequisitesResolveSkillLocalCLI`（7 个）
- 宿主机技能文件与软链接 `TestWriteHostSkillFilesRejectsEscapes`、`TestHostVersionDirSymlinksAreNotFollowed`（2 个）
- MCP 服务更新 `TestUpdateMCPService_AppliesNonScalarUpdateWithoutName`（1 个）

问答 SSE 不发 `complete`、连接不关闭（与 OpenSearch 无关；根因已定位为本机时钟问题，见下）：

- 现象：普通问答（快速回答路径）的 answer 事件都已发出，服务端日志显示 `Knowledge QA service completed`（`internal/handler/session/qa.go:1377`）、消息已持久化（`is_completed=t`）、`[PIPELINE] stage=Stream action=channel_close`，但 SSE 循环 `handleAgentEventsForSSE`（`internal/handler/session/stream.go:331` 起）一直没读到 `complete` 事件，直到客户端断开。Postgres 和 OpenSearch 知识库上都能复现（2026-11-03 再次复现：session `9eab2504…`）。之前的 Q-NEG 那一次正常结束，所以是间歇或与回答内容有关。
- 已排除：
  - 事件总线：`qa.go:757` 使用 `event.NewEventBus()`，是同步模式（`internal/event/event.go:165`），`completeQuickAnswerTurn`（`qa.go:1955`）中 `EventAgentComplete` 的处理（`agent_stream_handler.go:713 handleComplete`）先于 `publishCompletion`（`agent_stream_handler.go:930`）执行，不存在先后竞争。
  - 消息 ID 不匹配：`completeQuickAnswerTurn` 用 `streamCtx.assistantMessage.ID`，与 `setupStreamHandler`（`qa.go:825`）的 assistantMessageID 一致。
  - 写入失败：`completeStreamAssistantMessage`（`qa.go:2009` 一带）在 `publishCompletion` 出错时会记录 `Append persisted message completion failed`，复现时没有这条日志；消息持久化也成功，没有走错误分支。
  - SSE 读循环本身：读到 `complete` 就会结束（`stream.go:398` 一带）。
- 主要怀疑（待验证）：`internal/stream/redis_manager.go` 的 `GetEvents` 在 `json.Unmarshal` 失败时静默跳过该事件，但 offset 照常前进（约 135 行起）。如果 `complete` 事件的 `Data`（含 `final_content`、`usage` 等）反序列化失败，读端会永久错过它，而且不留日志。其次怀疑 `publishCompletion` 执行时 `completionEvent` 已经为 nil，例如 `handleComplete` 中途 return 或被重复调用。复现后 Redis 中该消息的事件 key 已不存在（TTL 已过或未命中 key 前缀），所以没能直接查看原始事件。下一步：在 `GetEvents` 的反序列化失败处加日志，或在 `publishCompletion` 记录是否真的写入，再复现一次。
- **根因（2026-11-04 定位，本机环境问题，不是代码缺陷）：** WSL 虚拟机的系统时钟每隔几秒在两个时间之间来回跳，相差 57,583 秒（约 16 小时，即 Windows 时区设为太平洋时间导致的偏差）。在 Redis 容器里连续执行 `TIME`，与 Windows 时间的差值在 0 和 57,583 秒之间交替。时钟往前跳时，Redis 会立即判定所有 1 小时 TTL 的 key 过期：`expired_keys` 累计 18.8 万，已完成消息的 stream key 的 TTL 反常地显示为 61,044 秒。问答进行中，SSE 事件列表 `os::<session>:<message>` 被中途删除，之后的事件写进一个从下标 0 重新开始的新列表；读端此时已经停在 offset 37，再也读不到后面的事件和 `complete`，事后用 continue-stream 也查不到这条消息的事件。插桩确认：`publishCompletion` 确实成功写入了 complete，读端停在 37 之后再没有拿到新事件。同一机制也会让 asynq 任务、live-run 标记等 Redis key 提前失效。
- 处理：代码不改行为，只保留两处诊断日志：`redis_manager.go GetEvents` 丢弃无法解析的事件时告警；`publishCompletion` 没有可发布的 complete 时告警。修复需要用户把 Windows 时区改为 UTC+08:00 北京（或修正 WSL 的时间同步），属于系统设置，由用户操作。时钟稳定之后，应复测确认问答都能正常收到 complete。
