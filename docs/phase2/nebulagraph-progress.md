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

## NG-D、NG-E — NOT_RUN
