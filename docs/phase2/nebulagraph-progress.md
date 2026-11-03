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

## NG-D、NG-E — NOT_RUN
