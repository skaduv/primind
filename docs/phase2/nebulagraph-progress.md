# NebulaGraph 二期进度记录

分支 `claude/ng-phase2-1b5ec0`（从 main 切出）。适配器来自 `codex/phase2-ng-01@2ed922d`，系统接线参考 `feat/nebula-graph@317953d` 并按 NG-02 草案的选择矩阵重写。原始 Spec：`docs/specs/phase-2-ai-development/04-nebulagraph.md`（未跟踪）。

状态取值：PASS / PARTIAL / BLOCKED / NOT_RUN / FAIL。证据只保留在本机临时目录，不入库；本文件记录命令和结论。

## 本机环境与隔离

| 项 | 值 |
| --- | --- |
| NebulaGraph | WSL Docker v3.8.0，graphd `127.0.0.1:9669`，root/nebula（未开鉴权），1 storaged，partition 10 / replica 1 |
| 本分支 Space | `primind_ng_contract`（契约/会话测试）、`primind_ng_perf`（性能/机制）、`primind_ng_app`（应用），VID `FIXED_STRING(1024)` |
| 应用隔离 | PostgreSQL 库 `primind_ng`，Redis DB 2 前缀 `ng:`，MinIO bucket `primind-ng`，端口 8082；配置写在未跟踪的 `.env.local` |
| 共用主机 | OS 会话同时在本机运行（OpenSearch/PG/Redis 共用），性能数据含这部分背景负载 |

### Windows 本机跑 Go 测试的注意事项

- `sqlite-vec` 的 cgo 需要 `sqlite3.h`：把 `go-sqlite3@v1.14.24/sqlite3-binding.h` 复制为某目录下的 `sqlite3.h`，设 `CGO_CFLAGS="-I<该目录> -Wno-deprecated-declarations"`。
- `gojieba` 在 Windows 动态链接下 init 崩溃（0xc0000005），测试需加 `-ldflags="-extldflags=-static"`。
- 基线：`internal/handler` 4 个、`internal/application/service` 约 60 个测试在 main 上同样失败（DataSource/Sandbox/Skill/MCP 等，与图无关），本分支失败集合与 main 完全一致。

## NG-A 合成实现 — PASS

- 适配器 `internal/application/repository/retriever/nebula/**` 与 `go.mod/go.sum` 来自 codex 分支；未搬 `outputs/`、`docs/`。
- 新增 `nebula/env.go`：`ConfigFromEnv` 读取 `NEBULA_*`，覆盖 Config 的全部地址/认证/TLS/预算/超时字段；`NEBULA_AUTO_SCHEMA`（默认 true）只在空 Space 建 Schema。
- 新增 `internal/types/graph_backend.go`、`internal/container/graph.go`；五处耦合点改读统一解析结果。
- 验证：`go build ./...`；`go test ./internal/types -run GraphBackend`、`./internal/container -run 'Graph|Nebula'`、`./internal/application/repository/retriever/nebula/`、`./internal/application/service/`（失败集合同 main）。

## NG-B NG-01 收口

| 项 | 状态 | 验证方式 | 结果 |
| --- | --- | --- | --- |
| 真实写/查/删契约（Q1–Q10、S1、R1、B-11、D1/D2、unknown-kb、C1 双进程来源并发 3 轮） | PASS | `TestNebulaRealContract`（`PRIMIND_NG_E2_ENABLE=authorized-isolated-run`，profile 指向 `primind_ng_contract`） | 5 次运行 4 次全过；首次（刚建 Schema 后）C1 第 2 轮子进程写入遇 `-1002`（会话失效），适配器按设计不重放写入并报错。后续 4 次连续通过 |
| P1 批次双限 | PASS | `TestLocalRealBatchDualLimits`（BatchRecords=7、StatementBytes=4096） | 60 节点拆成 64 条语句，每条均 ≤7 条记录且 ≤4096 字节，读回 60/59 |
| P3 共享写许可/有界等待 | PASS | `TestLocalRealSharedAdmission`（Connections=3、Writes=1、Waiters=2，8 个并发 AddGraph） | 2 成功、6 个明确 `ErrCapacity`，peak=1、peakWrites=1，成功者读回完整 |
| P4 取消 | PASS | `TestLocalRealCancellationStopsLaterBatches`（40ms 截止，1500 节点） | 返回 `context deadline exceeded`，返回后语句数不再增长，许可/计划内存归零 |
| 会话恢复 | PASS | `TestLocalRealKilledSessionRecovery`（另一 Space 的连接 `KILL SESSION` 掉被测池的会话） | 首个调用明确返回 `-1002`，坏会话被丢弃，下一调用恢复；调用方重放 AddGraph 幂等 |
| P6 持续运行 | PASS（仅测量） | `TestNebulaPerformance` soak 120s，batch=100、并发 4 | 13,840 次 AddGraph / 83,040 批，0 错误，peak 连接 5 ≤ 8、peakWrites 4 ≤ 4，结束后在途/等待/计划内存归零，清理后无残留 |
| P7 纯写基线 | PASS（仅测量） | 同上，batch 50/100 × 并发 1/2/4，每组 3 轮 + 预热；每轮 40 次调用 × 10 组实体对 | 见下表；全部轮次 0 错误、读回对账通过、清理无残留 |
| 业务性能目标 | BLOCKED | — | 业务目标未冻结，P7 只是纯图存储测量 |

P7 各组均值（AddGraph/s；调用 p50/p95 毫秒；批 p95 毫秒）：

| batch/并发 | new | replay | hotspot | new 调用 p50/p95 | new 批 p95 |
| --- | --- | --- | --- | --- | --- |
| 50/1 | 29.7 | 47.2 | 170.0 | 31.7 / 50.0 | 10.9 |
| 50/2 | 51.3 | 82.3 | 305.5 | 35.9 / 57.5 | 12.2 |
| 50/4 | 104.2 | 175.7 | 642.9 | 38.5 / 62.2 | 10.7 |
| 100/1 | 41.2 | 48.5 | 192.6 | 23.4 / 30.3 | 6.3 |
| 100/2 | 72.3 | 96.3 | 387.5 | 25.8 / 37.1 | 7.1 |
| 100/4 | 97.0 | 153.7 | 729.0 | 39.8 / 58.6 | 12.8 |

可查询（读回对账）耗时 0.69–0.88s/轮；Nebula 三容器 CPU 峰值 ≤25.5%（8 vCPU 归一）。

资源采样：Windows 侧循环（Windows 时钟打点）调用 WSL 内 `docker stats`/`df`/`ss` 与 PG 建连耗时；WSL 时钟与 Windows 相差约 16 小时，直接用 WSL 时间戳会被判为过期样本。

real-env 机制测试运行方式：

```bash
NEBULA_IT_ADDRS=127.0.0.1:9669 NEBULA_IT_SPACE=primind_ng_perf NEBULA_IT_KILL_SPACE=primind_ng_contract \
NEBULA_IT_USERNAME=root NEBULA_IT_PASSWORD=nebula \
go test -count=1 -ldflags="-extldflags=-static" ./internal/application/repository/retriever/nebula/ -run TestLocalReal -v
```

修正：空 Space 上 `DESCRIBE TAG` 返回 `-1005` 而不是 `ErrSchema`，原“先连接、失败再建 Schema”的顺序无法触发。容器改为 AutoSchema 时先 `InitializeSchema`（它拒绝非空 Space），再 `NewRepository` 校验。

## NG-C NG-02 系统装配 — 进行中

选择矩阵（AC01）已按草案实现并单测：

| GRAPH_BACKEND | NEO4J_ENABLE | 结果 |
| --- | --- | --- |
| 未设置或空字符串 | `strings.ToLower(v)=="true"` | neo4j；否则 disabled（带空格的 ` true ` 仍为 disabled） |
| `disabled` / `neo4j` / `nebulagraph` | 任意 | 该值；与已设置的旧开关含义不同时记一次脱敏警告 |
| 其他任何值（含大写、带空格、`nebula`） | 任意 | 启动报错，不降级 |

决定：新字段只接受三个小写精确值；显式空字符串按“未设置”处理（compose 常以空值透传变量）。

| AC | 状态 | 验证方式 | 结果 |
| --- | --- | --- | --- |
| AC01 未知值 | PASS | 本机启动 `GRAPH_BACKEND=nebula` | 依赖注入阶段报 `invalid GRAPH_BACKEND "nebula": want one of disabled, neo4j, nebulagraph`，进程退出，不降级 |
| AC01/AC02 nebulagraph | PASS | 本机启动（`.env.local`，Space `primind_ng_app`） | 日志 `Neo4j not selected ... skipping driver`；空 Space 自动建 Schema 后连接成功；`/api/v1/system/info` 返回 `graph_database_engine: NebulaGraph` |
| AC01/AC02 disabled + 冲突 | PASS | 本机启动 `GRAPH_BACKEND=disabled NEO4J_ENABLE=true` | 一次脱敏警告 `GRAPH_BACKEND=disabled overrides legacy NEO4J_ENABLE (which implies neo4j)`；Neo4j/Nebula 均不连接；`graph_database_engine: Not Enabled` |
| AC02 禁用不积压 | PASS | `TestChunkExtractDisabledBackendSkipsWithoutWork` | 禁用时不入队；禁用前已入队的任务消费时零 LLM、零写图并释放 pending 槽位 |
| AC05 Wiki / AC06 Agent 回归 | PASS（单测） | `./internal/agent/tools -run 'KnowledgeGraph\|Graph'`、`./internal/application/service/chat_pipeline -run 'Entity\|Graph'`、`./internal/application/service -run Wiki` | 全部通过；Agent 仍经容器注入同一 `RetrieveGraphRepository` |
| AC03 真实抽图→Nebula→问答 | PASS | 见下“真实链路” | 实际模型抽出实体/关系写入 `primind_ng_app`，普通问答经 Nebula 召回并给出正确答案与原文引用 |
| AC04 图/文本召回可区分 | PASS（有限） | 服务日志 `search_entity.go` 与 `[PIPELINE] stage=SearchParallel` | 图召回 `7 nodes, 5 relations` → `entity_results=1`，文本 `chunk_results=9`，分开记录。测试文档只有 1 个 chunk，图命中的 chunk 与文本命中重合，合并去重后最终引用保留文本得分版本（`match_type=0`）；“图独有证据”题未覆盖 |
| AC04 图独有证据 | PARTIAL（对照组推翻“图独有”结论） | 多 chunk 文档 `xuanwu.md`（3 chunk）：“玄武项目”只在 chunk0，“所长葛清远”只在 chunk2（不含“玄武”）。以单文档抽图覆盖（承担/领导 标签与示例）重解析后，图中有 北辰研究所-承担-玄武项目、葛清远-领导-北辰研究所，北辰研究所来源含 chunk0/chunk2。问题“玄武项目的承担单位的所长是谁？” | 文本检索 14 条，chunk2（`abf4e536-…`）不在文本结果中；图检索 2 nodes/1 relation，`filterSeenChunk new chunkIDs count: 2`（文本未见过的 chunk）。回答“北辰研究所，所长葛清远”，并以内联引用 `chunk_id=abf4e536-…` 标出该图独有 chunk。观察：消息的 references 列表只存了 5 条文本命中，未列出这个图独有 chunk（模型上下文里有）；另一篇已取消文档的 chunk 仍出现在文本检索结果中。两项都在文本/引用链路，不属图后端 ；**对照**：同题在 `GRAPH_BACKEND=disabled` 下（实体抽取跳过、`entity_results=0`）回答仍引用 chunk2——问答管线的 Merge 阶段会拼接同文档相邻 chunk（该文档仅约 1100 字），chunk2 内容经文本命中的 chunk0 带入上下文。结论：图召回确实独立产出文本初检未命中的 chunk，但本题答案并非只能靠图得到。要严格证明需让答案 chunk 与问题实体相距足够远（超出合并窗口）的长文档，未做 |
| AC05 模型可用性 | PASS | 直连 47501 chat、47503 embedding 最小调用（Key 不输出） | 均 HTTP 200，Embedding 4096 维；图客户端就绪另由启动日志/系统信息验证 |
| AC07 业务端到端性能 | BLOCKED | — | 业务目标未冻结；单文档实测见下 |

### 真实链路（2026-11-03，用户 `ng-a`，KB `fcd13be2-…`，文档 `ebd21d3f-…`）

1. 测试资料：虚构的“星河精密第 44 周项目周报”（408 字，1 个 chunk），含人物/产品线/供应商/协议等实体及负责、供应、汇报、任职关系。KB 开启 `graph_enabled`，抽图样例与标签为 负责/供应/汇报/任职/合作，抽图模型 `builtin-company-chat`，Embedding `builtin-company-embedding`。
2. 第一次处理：上传后入队 1 个图任务，Qwen3-Coder-Next 用时 77s（769→546 tokens），span `nodes_added=17, relations_added=0`。Nebula 读回 17 个实体，来源均指向真实 KB/文档/chunk。模型这次没有输出关系（解析器没有丢弃任何组），属模型输出波动。
3. 重解析：span `nodes_added=19, relations_added=20`（179s，1271 tokens）。Nebula 读回 19 实体、18 条不同关系（如 陈立伟-负责-鲲鹏 AGV 产品线、苏州光栅科技-供应-GS-360、许明远-任职-苏州光栅科技），全部来源指向新 chunk `100ae451-…`，旧 chunk `e323123a-…` 的数据为 0 条（重解析无旧写残留）。
4. 普通问答（非 Agent）：“鲲鹏 AGV 产品线由谁负责？它的核心激光雷达传感器由哪家供应商提供，对方的客户经理是谁？” → 回答“陈立伟（总经理），技术负责人林晓舟；苏州光栅科技供应 GS-360；客户经理许明远”，与原文一致并引用 `xinghe-weekly.md`。管线总耗时 14.6s。
5. 读回方式：nebula-console `LOOKUP ON primind_entity/primind_relation/primind_source WHERE <tag>.kb == "<base64url(kb)>"`，再 base64url 解码（适配器所有业务属性均为 base64url 编码）。

备注：Windows 上 curl 以中文文件名 multipart 上传会被拒（“文件名包含非法字符”，疑为 curl 编码），改用 ASCII 文件名；与图后端无关。

### 模型与运行准备（2026-11-03）

- `config/builtin_models.yaml` 已就位（被 `.git/info/exclude` 忽略）：问答/抽图 47501、视觉 47502、Embedding 47503（4096 维）、重排 47504（本机不可达，不选用）。启动日志确认内置模型已 upsert。
- `.env.local` 追加 `PRIMIND_LOCAL_{CHAT,EMBEDDING,VISION,RERANK}_API_KEY=` 空占位，以及含 `172.16.16.120` 的 `NO_PROXY/no_proxy`（本机代理访问该地址返回 502）。
- `AUTO_MIGRATE=true`（`.env`）。
- `primind_ng` 库：测试账号 `ng-a` 设为系统管理员（直接改 `users.is_system_admin`，仓库无引导管理员的入口）；系统设置 `ssrf.whitelist` 由空改为 `["172.16.16.120"]`，环境变量 `SSRF_WHITELIST_EXTRA=127.0.0.1` 不变。
- 向量检索（`RETRIEVE_DRIVER=postgres`）：`embeddings.embedding` 是不定长 `halfvec`，4096 维可存可算（pgvector 0.8.4 实测）。但现有 HNSW 只有 798/1024/3584 三个按维度的部分索引，应用不会为新维度建索引，且 halfvec HNSW 上限 4000 维，所以 4096 维检索走精确顺序扫描。本机少量文档可用；数据量大时需要换维度（若模型支持截断）或换检索后端。

## NG-D NG-03 生命周期

本节未修改任何业务代码（含 `knowledgebase.go`）：现有删除/取消/重解析流程经 `RetrieveGraphRepository` 调到 Nebula 适配器即可满足下列结果。

| AC | 项 | 状态 | 验证方式 | 结果 |
| --- | --- | --- | --- | --- |
| AC02 | 删文档 | PASS | 同 KB 两篇文档（共有实体名 宁海动力/青鸾分拣机），`DELETE /knowledge/<doc1>` | doc1 实体 19→0、其关系/来源全部清除；doc2 的 9 实体/6 关系/9 来源不变，同名实例互不影响 |
| AC02 | 删知识库 | PASS | 新建 KB 上传一篇后 `DELETE /knowledge-bases/<kb>` | 该 KB 图数据 14→0；另一 KB 24 行不变 |
| AC02/AC04 | 重解析 | PASS | `POST /knowledge/<id>/reparse` | 新 attempt 写入 19 实体/18 关系，全部指向新 chunk；旧 chunk 数据 0 条 |
| AC04 | 抽图进行中取消 | PASS | 图 span `running` 时 `POST /knowledge/<id>/cancel-parse` | 任务 ctx 取消，进行中的 LLM 请求 `context canceled`，未调用 AddGraph；Nebula 中该文档 0 行；`parse_status=cancelled`、`pending_subtasks_count=0` |
| AC03 | 跨租户权限 | PASS（修复后） | 租户 B 普通问答传入租户 A 的 `knowledge_base_ids`；对照组为 A 本人同题 | 修复前：文本检索 0 条，但 `search_entity.go` 信任请求 KB 列表，图检索命中 A 的 chunk 并进入 references（FAIL）。修复：cherry-pick OS 分支 `9ba3f7b`“安全修复：跨租户 KB 越权”（问答入口 `session_qa_helpers.go` 过滤无读权限的 KB，`search_entity.go` 只在授权范围内图检索）。修复后真实 Nebula 复测：B 无检索调用、无 references、回答“资料中未提及”，A 的内容出现 0 次；A 对照组仍有图召回（7 nodes/6 relations，`entity_results=2`）和 references。修复在共享代码上，Neo4j 后端走同一路径，一并生效 |
| AC04 | 过期 attempt 不写旧图 | PASS（修复） | `TestChunkExtractSupersededAttemptWritesNothing`（单测，红→绿） | 发现缺口：图任务只在开始时检查 attempt，LLM 调用期间若开始重解析（新 attempt 已先 DelGraph，且旧 chunk 在 finalizing 期间仍存在），旧任务返回后仍会 AddGraph 留下旧数据。修复：`extract.go` 在 AddGraph 前再查一次 attempt，被取代则丢弃结果，并以 superseded 标记收尾，不消耗新 attempt 的 pending 槽位。去掉该检查时“抽取期间被取代”用例失败（adds=1），恢复后通过 |
| AC05 | 写图失败重试 | PASS | `TestChunkExtractGraphWriteFailureIsRetriedNotSwallowed`（单测）；适配器级重放见 NG-B 会话恢复测试 | 写图失败作为任务错误返回（由 asynq 重试，MaxRetry 3），不吞错；重试成功后只写一次。适配器本身不重放写入，结果未知的写由上层重试，AddGraph 幂等 |
| AC05 | 运行期连接失败 | PARTIAL（待用户放行） | 适配器真实测试：会话被 KILL、截止时间（见 NG-B） | 应用运行中 docker pause graphd 的实测待用户放行（自动权限拦截，未尝试绕过） |

## NG-E NG-04 双后端回归（本机无 Neo4j，只验 disabled / nebulagraph）

| AC | 状态 | 验证方式 | 结果 |
| --- | --- | --- | --- |
| AC01 共同业务契约 | nebulagraph PASS；Neo4j **BLOCKED（未覆盖）** | NG-B 真实契约测试（Q1–Q10/S1/R1/C1/D1/D2） | 本机未部署 Neo4j，按约定不补；Neo4j 端契约回归未覆盖 |
| AC02 旧配置与显式切换 | PASS（disabled/nebulagraph） | 同一库 `primind_ng` 先以 `GRAPH_BACKEND=disabled` 启动，再切回 `nebulagraph`；单测矩阵含旧 `NEO4J_ENABLE` 全部组合 | disabled：不建 Neo4j 驱动、不连 Nebula，`graph_database_engine=Not Enabled`，问答日志 `skipping extract entity, knowledge graph is disabled`、`entity_results=0`，文本链路照常回答。切回 nebulagraph：连接已有 Space（不重建 Schema），`graph_database_engine=NebulaGraph`，同题图检索 `2 nodes, 1 relations`、`entity_results=2`。状态字段不冒充另一后端 |
| AC03 切回覆盖说明 | PASS（说明） | — | 无双写、无迁移：选 disabled 期间新增/修改/删除/重解析/移动的文档不会写入或清理 Nebula，旧图数据原样保留；切回 nebulagraph 后，期间删除的文档可能留下已失效来源（问答侧由 chunk/文档存在性过滤，不进入上下文），期间新增的文档没有图数据，需重解析补图。从 Neo4j 切到 Nebula（或反向）同理不迁移，需对受影响 KB 重解析重建 |
| AC04 交付演练前置 | NOT_RUN | — | 切换时点、暂停写入范围、补图/对账步骤归交付阶段 |
| AC05 Wiki/Agent | PASS（单测） | NG-C 已跑的 Agent 图工具、Wiki 相关单测 | 实体图改造未影响 Wiki；Agent 仍注入同一图仓库 |
