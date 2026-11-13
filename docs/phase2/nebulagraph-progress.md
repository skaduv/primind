# NebulaGraph 二期进度记录

分支 `claude/ng-phase2-1b5ec0`（从 main 切出）。适配器来自 `codex/phase2-ng-01@2ed922d`，系统接线参考 `feat/nebula-graph@317953d` 并按 NG-02 草案的选择矩阵重写。原始 Spec：`docs/specs/phase-2-ai-development/04-nebulagraph.md`（未跟踪）。

状态取值：PASS / PARTIAL / BLOCKED / NOT_RUN / FAIL。证据只保留在本机临时目录，不入库；本文件记录命令和结论。

## 最终 AC 状态总表（2026-11-04）

基线：本地 `main`（`fc23efc`，领先 `origin/main`）；本分支无需 rebase。收尾验证：`go build ./...` 通过；`nebula`、`chat_pipeline`、`container`、`types` 包测试全部通过；`internal/application/service` 失败 58 个，与 main 同一集合（均与图无关）。未推送、未开 PR。

| AC | 状态 | 提交 | 未覆盖 / 备注 |
| --- | --- | --- | --- |
| NG-01-AC01 契约/夹具可重复 | PASS | c6550be | — |
| NG-01-AC02 版本/SDK/Schema/身份 | PASS | c6550be | v3.8.0 + nebula-go v3.8.0 |
| NG-01-AC03 真实写查删闭环 | PASS | 84d5f05 | 首次刚建 Schema 后 C1 出现一次 `-1002`，后续 4 次连过 |
| NG-01-AC04 同名/多来源/方向/删除风险 | PASS | 84d5f05 | — |
| NG-01-AC05 运行证据 | PASS | 84d5f05 | 证据在本机临时目录，不入库 |
| NG-01-AC06 批量/池/并发/取消 + P6/P7 | PASS（仅测量） | 84d5f05 | 业务性能目标未冻结 |
| NG-02-AC01 选择矩阵 | PASS | c6550be, 7b69387 | 显式空值按未设置处理（本分支决定） |
| NG-02-AC02 五处耦合/禁用不积压 | PASS | c6550be, 7b69387 | — |
| NG-02-AC03 真实抽图→Nebula→问答 | PASS | 7b69387 | 模型输出有波动（一次抽取 0 关系） |
| NG-02-AC04 图/文本召回可区分 | PASS（严格证明） | 7b69387, 05afbc9, 5c85053, 本次提交 | 首轮单文档题被对照组推翻（相邻 chunk 合并、文档摘要泄露答案）；重设计后 disabled 答不出、nebulagraph 答对，见下“AC04 严格证明” |
| NG-02-AC05 模型可用性/字段兼容/Wiki | PASS | 7b69387 | — |
| NG-02-AC06 Agent 注入 | PASS（单测） | 7b69387 | Agent 真实链路未跑 |
| NG-02-AC07 业务端到端性能 | BLOCKED | 7b69387 | 业务目标未冻结；4096 维向量无 HNSW 索引（走顺序扫描） |
| NG-03-AC01 重复/并发/部分失败 | PASS | 84d5f05 | 适配器级（C1、重放幂等） |
| NG-03-AC02 删除/重解析/移动不留残留 | PASS（删文档、删 KB、重解析） | ecfe7cd | 移动/复制未测 |
| NG-03-AC03 权限边界 | PASS（修复后） | 8266191, 6b69618 | 跨租户越权由 OS 会话修复后 cherry-pick；组织共享场景只有单测 |
| NG-03-AC04 取消/过期不污染新图 | PASS | ecfe7cd, 62c4a00 | 发现并修复“抽图期间被重解析取代仍写图” |
| NG-03-AC05 失败可定位/重试有界 | PASS | 84d5f05, 62c4a00, 本次提交 | graphd 运行中暂停约 25s 实测：删除时写图失败明确报错，asynq 约 50s 后重试成功清理；恢复后首次图检索即成功，无需重启应用；应用清理关闭 Nebula 连接池（graphd 会话 10→14→10） |
| NG-03-AC06 Agent/共享/移动路径 | PASS（真实） | 5e65515, df2c9fa, 621f39c, 本次提交 | 同 store 移动、知识库复制修复后真实验证通过；Agent 真实图检索通过（模型汇总时漏掉一条已返回的关系，见下）。仅本地单测覆盖：组织共享；未测：跨 store 移动（reparse 模式，走重解析链路） |
| NG-04-AC01 双后端共同契约 | PASS（nebulagraph 与 neo4j） | 84d5f05, 5c85053, 工作区未提交 | 同一套 Q1–Q10/S1/R1/C1/D1/D2 夹具在两个后端都通过；Neo4j 2026.10.1（APOC 2026.10.1）+ 驱动 v6.0.0 |
| NG-04-AC02 旧配置/显式切换 | PASS（neo4j、nebulagraph、disabled 三者） | 5c85053, 工作区未提交 | 真实启动矩阵含 Neo4j：旧 `NEO4J_ENABLE=true`、显式 `GRAPH_BACKEND=neo4j`、两种冲突配置均正确，系统信息引擎名正确，见 NG-E |
| NG-04-AC03 切回覆盖说明 | PASS | 5c85053 | — |
| NG-04-AC04 交付演练前置 | NOT_RUN | — | 归交付阶段 |
| NG-04-AC05 Wiki/Agent/无 mock 冒充 | PASS | 5c85053 | — |

已知问题（不在图后端，留给后续）：references 列表不含图独有 chunk；已取消文档的 chunk 仍可被文本检索命中。

## 本机环境与隔离

| 项 | 值 |
| --- | --- |
| NebulaGraph | WSL Docker v3.8.0，graphd `127.0.0.1:9669`，root/nebula（未开鉴权），1 storaged，partition 10 / replica 1 |
| 本分支 Space | `primind_ng_contract`（契约/会话测试）、`primind_ng_perf`（性能/机制）、`primind_ng_app`（应用），VID `FIXED_STRING(1024)` |
| 应用隔离 | PostgreSQL 库 `primind_ng`，Redis DB 2 前缀 `ng:`，MinIO bucket `primind-ng`，端口 8082；配置写在未跟踪的 `.env.local` |
| 共用主机 | OS 会话同时在本机运行（OpenSearch/PG/Redis 共用），性能数据含这部分背景负载 |

> **存储格式 v2（2026-11）**：上表是 v1 时期的记录。v2 起属性改存明文、VID 改为 `ng2_` 内容哈希，图空间须为 `vid_type=FIXED_STRING(32)`；`FIXED_STRING(1024)` 的旧空间会在启动时被拒绝（`ErrSchema`），不能原地升级，需新建空间并重新抽图。`NEBULA_AUTO_SCHEMA=true` 仍只在空 Space 建 Schema。详见 `internal/application/repository/retriever/nebula/CONTRACT.md`。

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
| AC04 严格证明（重做） | PASS | 新 KB `c7fa62f1…`（Postgres 向量）：关键文档 `key.md`（2 个 chunk，单文档关闭摘要）：chunk0 含“玄武项目由北辰研究所承担”，chunk1 含“北辰研究所的掌门人是葛清远”（与问题无“玄武/项目/单位/所长”任何字面重合，且长度 ≥350 字不触发相邻扩展）；另上传 5 篇台账类填充文档（35 个 chunk，按文件关闭抽图），内容为其他项目/承担单位/所长，把答案 chunk 挤出文本前 30（hybrid-search 中排第 39/43）。图中有 北辰研究所-承担-玄武项目、葛清远-领导-北辰研究所，北辰研究所来源含两个 chunk。问题“玄武项目的承担单位的所长是谁？” | **nebulagraph**：图检索 2 nodes/1 relation，`filterSeenChunk new chunkIDs count: 2`，`chunk_results=30 entity_results=2`；回答“北辰研究所，所长葛清远”，并引用“三、人物介绍…掌门人是葛清远”（只在答案 chunk `454dd633`）。**disabled 对照**：`skipping extract entity`，`entity_results=0`；回答找不到所长，未出现“葛清远”，还误把填充资料里的“刘思远”当作答案。过程中发现两处会让文本侧拿到答案、已在设计中规避：① 文档摘要 chunk 会复述答案；② 小于 350 字的命中 chunk 会被相邻 chunk 扩展。另：问答时的实体抽取偶尔返回空（一次），此时图检索被跳过 |
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
| AC05 | 运行期连接失败 | PASS | 用户放行后单独执行 `wsl.exe -e docker pause primind-dev-nebula-graphd`，约 25s 后单独执行 unpause（graphd 随即 running healthy、9669 可连）。暂停期间：问答一次、删除 xuanwu 文档（KB1）一次 | 删除任务的图清理在约 5s 后失败并明确记录 `DeleteKnowledge delete knowledge graph failed … nebula execution failed (result may be unknown)`；asynq 约 50s 后重试，`successfully deleted 1 knowledge items`，该文档 Nebula 行 7→0、chunk 0、无残留。问答的实体检索恰在恢复后执行，直接返回 5 nodes/4 relations，未重启应用即重连。无旧数据写入、无丢写 |
| AC05 | 退出时关闭连接池 | PASS | `TestRealNebulaPoolClosedByResourceCleaner`（容器包真实测试，经应用的 `initGraphRepository` 与 `ResourceCleaner` 建池与关闭；需 `NEBULA_IT_*`），测试期间用 nebula-console 统计 `SHOW SESSIONS` | graphd 会话数：测试前 10、池打开时 14、`Cleanup` 后 10；之后调用返回 `ErrClosed`。说明：Windows 上用 `taskkill /F` 强杀应用不会走清理，正常退出（信号）才会 |

## NG-E NG-04 双后端回归（本机无 Neo4j，只验 disabled / nebulagraph）

| AC | 状态 | 验证方式 | 结果 |
| --- | --- | --- | --- |
| AC01 共同业务契约 | PASS（nebulagraph 与 neo4j） | Nebula：NG-B 真实契约测试。Neo4j：新增 `TestNeo4jRealContract`（`retriever/nebula/contract_neo4j_integration_test.go`，同一套夹具与黄金集，经正式三方法；需 `PRIMIND_NEO4J_TEST_URI/_USER/_PASSWORD`，密码只在命令里注入） | 驱动：neo4j-go-driver v6.0.0 连接认证 Neo4j/2026.10.1 成功（Bolt 6.0）。首次因 neo4j-local 未装 APOC 阻塞（写入/删除依赖 apoc.merge.*、apoc.coll.union、apoc.periodic.iterate）；用户同意后启用 APOC 2026.10.1 重建容器。重跑：基线 9 实体/8 关系，Q1–Q10 结果数与黄金集一致（如 Q3 4 实体/5 关系，Q5/Q6/Q10 为空），Q7/Q8 无越界 chunk，S1、R1 幂等、C1 两个独立客户端并发追加来源、D1 重复删除、D2 全删均通过；测后 Neo4j 总节点 0 |
| AC02 旧配置与显式切换 | PASS（neo4j / nebulagraph / disabled） | 本机连续 4 次真实启动（Neo4j 密码只在启动命令中经命令替换注入，日志中出现 0 次）；disabled 两种组合见前 | `GRAPH_BACKEND=`(空)+`NEO4J_ENABLE=true`：`backend=neo4j`，建立 Neo4j 驱动，`graph_database_engine=Neo4j`。显式 `GRAPH_BACKEND=neo4j`（未设旧开关）：同上，无告警。`GRAPH_BACKEND=neo4j`+`NEO4J_ENABLE=false`：告警 `overrides legacy NEO4J_ENABLE (which implies disabled)`，仍走 neo4j，引擎 `Neo4j`。`GRAPH_BACKEND=nebulagraph`+`NEO4J_ENABLE=true`：告警 `which implies neo4j`，不建 Neo4j 驱动，连接 Nebula，引擎 `NebulaGraph`。三者之间切换只需改配置重启，状态字段不冒充另一后端 |
| AC03 切回覆盖说明 | PASS（说明） | — | 无双写、无迁移：选 disabled 期间新增/修改/删除/重解析/移动的文档不会写入或清理 Nebula，旧图数据原样保留；切回 nebulagraph 后，期间删除的文档可能留下已失效来源（问答侧由 chunk/文档存在性过滤，不进入上下文），期间新增的文档没有图数据，需重解析补图。从 Neo4j 切到 Nebula（或反向）同理不迁移，需对受影响 KB 重解析重建 |
| AC04 交付演练前置 | NOT_RUN | — | 切换时点、暂停写入范围、补图/对账步骤归交付阶段 |
| AC05 Wiki/Agent | PASS（单测） | NG-C 已跑的 Agent 图工具、Wiki 相关单测 | 实体图改造未影响 Wiki；Agent 仍注入同一图仓库 |

## 补充记录（2026-11-04）

### 同 store 移动后图数据缺失（已修，单测 + 真实验证）

- 实测：把 qingluan 文档从 KB1 `fcd13be2…` 以 `reuse_vectors` 移到同 store 的新 KB `e78a60ca…`（开启抽图）。移动完成后，源 KB 中该文档的图数据 9→0（源范围无残留，符合预期）；但目标 KB 中该文档图数据为 0，文档和 2 个 chunk 已在目标 KB。
- 定位：`knowledge_clone_move.go` 的 `moveKnowledgeReuseVectors` 删除源图命名空间后，没有在目标 KB 投递抽图任务，也没有迁移图数据。结果是移动后的文档在目标 KB 无法被图检索命中，需重解析才能恢复。
- 修复（主控选定第一种修法）：`moveKnowledgeReuseVectors` 删除源图命名空间并完成移动后，调用 `enqueueMovedGraphExtract`：目标 KB 开启抽图时（`ResolveProcessConfig(targetKB, overrides).GraphEnabled`），对移动过去的文本 chunk（经 `selectGraphChunks`）逐个投递 `NewChunkExtractTask`，任务带 KnowledgeID 与当前 attempt，沿用取消/删除/attempt 过期检查。不预置 pending 槽位：移动后的文档已是 completed，任务结束时的槽位释放在计数为 0 时是空操作（`FinalizeSubtask` 以 `pending_subtasks_count > 0` 为条件），不会把文档拉回 finalizing。图后端 disabled 时 `NewChunkExtractTask` 不投递。投递失败只记日志，不回滚已完成的移动。
- 验证：`TestEnqueueMovedGraphExtract`——目标开启抽图且 nebulagraph 时投递数 = chunk 数（3）；图后端 disabled 时 0；目标未开抽图时 0。原有移动/转移单测通过。真实 LLM 重抽未跑（额度原因）；会按 chunk 数产生 LLM 调用，移动大量文档时耗时与模型成本相应增加。改动文件：`knowledge_clone_move.go`（未改 `knowledgebase.go`）。
- 真实验证（2026-11-04，`5e65515` 构建）：把已抽图的 `key.md`（2 个 chunk）以 `reuse_vectors` 从 KB `c7fa62f1…` 移到同 store、开启抽图的 KB `e78a60ca…`。移动后日志投递 2 个图任务（chunk `3ea3aa57…`、`454dd633…`），两次抽取完成；源 KB 中该文档图数据 4→0，目标 KB 生成 9 行（含 葛清远-任职-北辰研究所）；文档 `completed`、pending 0。目标 KB 普通问答“葛清远在哪个机构任职？”：`entity search result count: 3 nodes, 2 relations`、`entity_results=2`，回答“北辰研究所”并引用目标 KB 下的 chunk `454dd633…`。抽取结果有模型波动：这次未抽出 北辰研究所-承担-玄武项目。

### Wiki 输出上限（已修，`36bc5e7`）

- 原问题：`wiki_ingest.go` 写死 `wikiLLMMaxTokens=32768`，本机问答模型上下文只有 32768，每次 Wiki 抽取都返回 400（输入+输出超出上下文），Wiki 自动生成全部失败（main 上即存在）。
- 修复：按模型 `context_window` 的 1/4 计算，夹在 [4096, 32768]；模型配置了 `max_output_tokens` 时不超过它；上下文未知时用 16384。
- 验证：单测 `TestWikiMaxTokensForContextWindow`、`TestGenerateWithTemplateSizesMaxTokensToModel`（32k 模型实际请求 8192）及原有 Wiki 相关单测通过。真实 Wiki 生成未跑（额度原因）。

### 知识库复制后没有图（已修，单测 + 真实验证）

- 现象：复制已抽图的 KB 后，副本 `indexing_strategy.graph_enabled=false`、`extract_config` 为空，图数据 0 行。
- 定位：`knowledgebase.go` 的 `CopyKnowledgeBase` 构建目标 KB 时未带 `IndexingStrategy`、`ExtractConfig`（副本被 `EnsureDefaults` 设为默认的向量+关键词）；`knowledge_clone_move.go` 的 `CloneChunk` 只复制 chunk 与向量，不复制、也不重建图数据。
- 修复（最小改动）：`CopyKnowledgeBase` 复制 `IndexingStrategy` 与 `ExtractConfig`（`cloneExtractConfig` 深拷贝标签/示例，避免两个 KB 共用切片）；`CloneChunk` 在新 chunk 落库后调用 `enqueueTransferredGraphExtract`（与移动修复共用，原名 `enqueueMovedGraphExtract` 改为通用名），目标 KB 开启抽图且图后端可用时按 chunk 投递抽图任务。`knowledgebase.go` 只在 `CopyKnowledgeBase` 的字段列表里加 2 行，不涉及 OS 会话改过的 `ProcessKBDelete`。
- 单测：`TestCloneExtractConfigCopiesWithoutAliasing`（复制保留开启状态、不共用切片）；`TestEnqueueMovedGraphExtract` 改调通用函数后仍通过。
- 真实验证：复制 KB `e78a60ca…`（含 qingluan、key.md）得副本 `37677598…`。副本 `graph_enabled=true`、`extract_config.enabled=true`；投递 3 个图任务（qingluan 1 + key.md 2），3 次抽取完成、0 失败；副本图数据 54 行，两篇文档 `completed`、pending 0。随后删除源 KB：源 KB 图数据 0，副本图数据仍 54 行、chunk 4 个；副本问答“青鸾分拣机的伺服电机由哪家公司供应？”：`entity search result count: 5 nodes, 4 relations`、`entity_results=1`，回答“常州驰远电气”并引用副本下的 chunk `6a6d7f31…`。
- 代价：复制会按 chunk 数重新调用抽图模型。

### 本地文件损坏（环境问题，已恢复）

- WSL 异常重启后，`internal/application/service/tenant_skill_host_test.go` 从第 4097 字节起有 4 KB 被写成乱码（文件大小与修改时间未变，所以 `git status` 未显示），导致 service 包测试编译失败。
- 处理：对全部 4550 个跟踪文件按内容哈希与索引比对，只有这一个文件损坏；已用 `git show HEAD:<文件>` 恢复，哈希与 HEAD 一致。未跟踪的 `.env.local`、`config/builtin_models.yaml` 检查无控制字符。

### Agent 问答走图检索（真实）

- 准备：内置 `builtin-smart-reasoning` 在本租户未设 `model_id`，不改内置配置，另建测试 Agent。含 `search_knowledge` 工具的 Agent 必须配置重排模型（`agentRequiresRerankModel`），本机重排服务不可达，所以测试 Agent 只开 `query_knowledge_graph`、`read_document`、`list_documents`（`b7ce2099…`，`model_id=builtin-company-chat`，KB 限定为复制出的 `37677598…`）。这样图工具是唯一的检索途径。
- 问题：“请使用知识图谱查询工具回答：常州驰远电气为哪个产品供应什么部件？这个产品还有哪些供应商？”
- 结果：Agent 3 轮调用 `query_knowledge_graph`（参数 `knowledge_base_ids=[37677598…]`、`query="常州驰远电气 产品 零部件 供应商"`），`execute_done success=true`。工具返回内容来自 Nebula，包含 常州驰远电气-供应-伺服电机/青鸾分拣机，以及 宁海动力-供应-青鸾分拣机。
- 回答：“常州驰远电气为青鸾分拣机供应伺服电机（销售代表孙一帆）”，引用副本 KB 下的 chunk `6a6d7f31…`，正确。但称“未显示其他供应商”——工具结果里已有 宁海动力，模型汇总时漏掉。属于模型推理问题，图检索与工具链路本身正确。
- 观察：Agent 只要启用 `search_knowledge` 就强制要求重排模型；本机环境因重排不可达，无法测“文本 + 图”混合的 Agent。

### Neo4j 后端应用级真实链路（2026-11-05）

- 启动：`GRAPH_BACKEND=neo4j`，`NEO4J_URI=bolt://127.0.0.1:7687`、`NEO4J_USERNAME=neo4j`，`NEO4J_PASSWORD` 只在启动命令里用命令替换注入（日志中出现 0 次）。日志 `backend=neo4j`，`/system/info` 为 `Neo4j`。
- 抽图写入：新建开启抽图的 KB `a1e28b29…`，上传 qingluan-memo.md（1 chunk）与 key.md（2 chunk），3 个图任务全部完成；Neo4j 中该 KB 标签下 19 节点、8 关系（qingluan 9/6，key.md 10/2）。
- 问答：租户 A 问“青鸾分拣机的伺服电机由哪家公司供应，销售代表是谁？”：`entity search result count: 4 nodes, 3 relations`、`entity_results=1`，回答“常州驰远电气，销售代表孙一帆”，引用 chunk `e86d8200…`，references 4 条。
- 跨租户：租户 B 传该 KB ID 问同一问题，references 0，回答“资料中未提及”，A 的内容出现 0 次。
- 删除：删除 qingluan 后该文档 Neo4j 节点/关系 0/0，key.md 仍为 10/2；删除 KB 后该 KB 标签下 0/0，Neo4j 全库节点 0（测试数据全部清理）。
- 未测：Neo4j 后端上的移动、复制（Nebula 已真实验证；两者都走 `enqueueTransferredGraphExtract` 后端无关的投递逻辑）。
