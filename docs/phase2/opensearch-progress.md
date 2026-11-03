# OpenSearch 二期进度记录

分支：`claude/os-phase2-9b4d6d`（从 main 切出）。验收 ID 来自 `docs/specs/phase-2-ai-development/03-opensearch.md` 第 5、6 节。
本记录只保留状态、验证方式、结果和未完成项，不另存证据文件。

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

状态：AC-OS02-01~04 已通过；AC-05（重启 OpenSearch 后数据保留）、AC-06（原后端与 Wiki 冒烟）见下文。

样本与问题：沿用 `integration/testdata` 中冻结的合成样本 `OS_SAMPLE_A.txt`、`OS_SAMPLE_B.txt`，以及 `oracle.json` 里的 Q-KW/Q-VEC/Q-HYB/Q-QA/Q-NEG。请求体用 UTF-8 文件发送（Git Bash 下把中文直接放在 curl 参数里会乱码）。

模型鉴权：`POST /initialization/remote/check`（chat）和 `POST /initialization/embedding/test` 只带 `modelId`，由服务端补全已保存的 Key，返回“连接正常”和“向量维度=4096”。应用重启后复核结果相同。

| AC | 结果 |
| --- | --- |
| AC-OS02-01 存储与绑定 | 用户 A 的 OpenSearch store `8896d60e…`（`http://127.0.0.1:9200`，index_name `primind_os_app`）绑定新知识库 `60e8a63c…`（Embedding `builtin-company-embedding`，向量+关键词）。上传样本后自动建索引 `primind_os_app_8896d60e-a60_4096_v1`（alias `…_4096`），并有真实写入和读取。 |
| AC-OS02-02 标识一致、维度、重复写 | 解析完成后，数据库 `chunks` 与 OpenSearch 文档 `_id` 逐条一致：每个样本 1 个 text + 1 个 summary，共 4 条，knowledge_base_id 一致，is_enabled=true。存储的向量长度 4096。Postgres `embeddings` 表中该知识库为 0 条，数据只在 OpenSearch。对样本 A `POST /knowledge/:id/reparse`：生成新 chunk ID，OpenSearch 与数据库仍逐条一致，旧 ID 全部消失，没有重复。 |
| AC-OS02-03 三路召回 | `POST /knowledge-bases/:id/hybrid-search`。Q-KW（关闭向量）：只命中 A 的 text 和 summary，match_type=关键词。Q-VEC（关闭关键词）：A 的两条排在前面（0.749/0.712），B 的两条在后（0.459/0.418），作为补充命中。Q-HYB：A 的 text 排第 1，结果中没有重复 chunk ID，沿用现有融合逻辑。所有结果都只来自本知识库。 |
| AC-OS02-04 问答引用 | 新会话 `POST /knowledge-chat/:session_id`，限定本知识库，问答模型 `builtin-company-chat`。Q-QA 的回答给出 SEAL_A_101 和 WO_A_001，注明来源 `OS_SAMPLE_A.txt`，正文引用 `chunk_id=2d3f12d8…`（A 的 text chunk，存在于 OpenSearch）；references 中是 A 的 text/summary 和 B 的补充片段。Q-NEG 回答资料未提及 COMP_C 及其部件，没有把 A/B 的部件安到 COMP_C 上。回答原文以数据库 `messages` 表为准。 |

问答流的问题（未定位）：Q-QA 两次请求，服务端日志都在约 21 秒内显示 `Knowledge QA service completed`、`channel_close`，但 SSE 没有发出 `complete` 事件，连接一直不关闭，直到客户端超时（590 秒）。Q-NEG 那次正常收到 `complete`。数据库中消息是完整的（`is_completed=t`）。是否与 OpenSearch 有关，待用原后端知识库对照。

准备情况（均为未跟踪的本机文件，不提交）：

- 模型：`config/builtin_models.yaml`（由 `.git/info/exclude` 忽略），指向公司内网 172.16.16.120 上的服务：问答 47501、视觉 47502、Embedding 47503（Qwen3-Embedding-8B，4096 维）、重排 47504（本机直连超时，知识库不选用）。Key 通过 `${PRIMIND_LOCAL_*_API_KEY}` 从 `.env.local` 读取，加载器 `LoadBuiltinModelsConfig` 会自行展开；`AUTO_MIGRATE=true`（来自 `.env`）。
- `.env.local` 追加了 4 个空的 Key 占位，`NO_PROXY`/`no_proxy` 包含 `172.16.16.120`（本机 HTTP(S)_PROXY 走 7890 会返回 502）。
- SSRF 白名单：本库的测试用户不是系统管理员，系统设置 API 返回 403。我没有通过 SQL 提权，改为在 `.env.local` 设置 `SSRF_WHITELIST_EXTRA=127.0.0.1,172.16.16.120`；应用启动时会把它与系统设置里的白名单合并（`applySSRFWhitelist`），原有条目保留（当前系统设置为空）。

4096 维核对：

- 适配器允许的维度为 1..16000；`TestLocalRealBaseline` 以 dim=4096、lucene HNSW 实测 2000 条：写入约 268 chunk/s，向量检索 p95 78.2 ms，关键词检索 p95 27.9 ms，计数一致。
- 导入路径没有问题：索引器每批 40 条，约 1.8 MB，低于 bulk 10 MB 上限。
- **问题：知识库复制在 4096 维下会失败。** `CopyIndices` 每扫一页 500 条，就用一次 BatchSave 写入；4096 维估算单条约 21.6 KB（真实 JSON 约 45 KB），超过 10 MB 上限，返回 `ErrBatchTooLarge`。真实集群临时复现：复制 320 条全部未发送（`320 not sent`），目标为 0 条，错误会明确返回，不会静默。按当前上限，大约超过 220 条的知识库就无法复制。**已修复**，见 OS-C“4096 维知识库复制”。
- SSRF：系统设置白名单这条路径没有验证，原因是测试用户不是系统管理员（不通过 SQL 提权）；本机改用 `SSRF_WHITELIST_EXTRA`。

## OS-C 生命周期（AC-OS03-01~03）

状态：适配器层已在真实集群验证并修复一个缺陷；应用级（重解析、删文档/知识库任务、移动/复制入口）待模型。

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

待做（需模型）：通过应用入口重跑修改/重解析、停用、标签、移动、复制、删除文档、删除知识库，核对 OpenSearch 中无残留、不误删，以及异步任务失败不被当作删除完成。

另记：`MoveKnowledgeIndices`、停用/标签更新、无维度关键词检索使用 `<base>_*` 通配。env 路径的 base 是裸 `primind`，可能匹配到同集群其他 store 的索引；因为过滤条件是唯一的 chunk/KB ID，实际不会串数据，暂不处理。

## OS-D 权限（AC-OS03-04）

状态：ID 越权与共享/撤销已在真实应用验证；检索命中内容级复核待模型。

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

未完成：有模型后，用已入索引的样本重跑上表，核对 hybrid-search 返回的 OpenSearch 命中在越权/撤销时为空，共享时只包含被共享知识库的片段。

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
| AC-OS03-08 应用重启恢复 | 注册恢复已验证；优雅退出未覆盖 | 真实应用：用户 A 建 OpenSearch store（`8896d60e…`，`POST /vector-stores/:id/test` 成功），重启后日志 `Loading 1 vector store(s)` → `Registered DB vector store … engine=opensearch`。Windows 下只能强杀进程，没有走到优雅退出的 cleanup；进程级资源由进程退出回收。 |

待办/风险：

- `knowledge_process.go` 的 BatchIndex 失败分支按 knowledge ID 删除 chunk 和索引；如果旧 attempt 的失败晚于新 attempt 写入，可能误删新数据（对所有后端都成立，不限 OpenSearch）。需要用模型跑真实重解析交错来复现，确认后再给最小方案。
- 显式 `number_of_replicas: 0` 因 `omitempty` 和 `config.go` 默认值投影不会生效（spec 候选缺陷），单节点上新索引副本未分配（yellow），不影响读写；是否修复待定。

## OS-F 性能（AC-OS02-07、AC-OS03-09）

状态：适配器层基线已测；含 Embedding/LLM 的应用级基线和业务目标值待模型与用户确认。

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

这只是纯索引和检索子路径的基线，不能关闭 AC-OS02-07 或 AC-OS03-09。待做：应用级导入（解析、Embedding、索引确认、可检索）和三路检索、问答的分段耗时，并发写查混合，以及向用户确认业务目标值（规模、导入窗口或最低吞吐、p95、错误率、资源上限）。

## 与 NebulaGraph 会话的共享文件

- `internal/application/service/knowledgebase.go`：暂未改动。
