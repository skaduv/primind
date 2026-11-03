# OS-02 验证入口

本目录是测试 harness，复用正式 `NewOpenSearchClient`、`NewRepository`、`Save`、`BatchSave`、`Retrieve` 和 `RequestBudget`。不接入应用启动，不新增索引器，不修改公共 API、RRF、过滤、默认后端或依赖。当前交付只有本地 E1；没有真实端点、模型、重启、清理或压测授权。

## 本地验证

在 `D:/work-tree/work-tree/10-项目/03-公司项目/primind-opensearch` 执行，每次一个 Go 命令：

```powershell
$env:GOMAXPROCS='2'
$env:PRIMIND_OS02_OPT_IN=''
$env:PRIMIND_OS02_CONFIG=''
go test -mod=readonly -p 1 ./internal/application/repository/retriever/opensearch/integration -count=1 -v -timeout=5m
go test -mod=readonly -p 1 ./internal/application/repository/retriever/opensearch/integration -count=1 -v -timeout=5m -race
go test -mod=readonly -p 1 -tags integration ./internal/application/repository/retriever/opensearch/integration -run '^TestReal' -count=1 -v -timeout=5m
```

第三条在未授权配置下应明确 `SKIP / NOT_RUN`，不能计为真实验收通过。普通测试只有 loopback `httptest`，不读取真实服务环境。报告目录的 `run-test.ps1` 清空真实 opt-in，保留原始输出和实际 Go 退出码，另有包含构建的 600 秒外层限制。

## 获准后才使用的真实入口

复制报告目录中的 `replay-config.template.json` 或 `performance-config.template.json` 为本次批准的独立配置，填写所有 `null`，保存并计算 SHA256。模板故意不能执行。配置不含密码，只允许 `OS02_SECRET_*` 进程环境引用；由批准的秘密管理方式注入。不得把凭据写进命令、报告或 Git。

启动需要同时满足：

1. `-tags integration` 和精确 `-run` 测试名。
2. `PRIMIND_OS02_OPT_IN=OS-02:replay` 或 `OS-02:performance`，与配置 mode 完全一致。
3. `PRIMIND_OS02_CONFIG` 指向该次审批后的 JSON 文件。缺配置时，在读文件、凭据或 DNS 之前退出。
4. 有效期覆盖整个有限窗口（不超过 24 小时），服务自报 `2.19.7`、明确 cluster UUID、身份/TLS、业务对象、全量 index config、alias 和物理索引、静止核对范围。
5. `go test -timeout` 至少大于配置 `max_seconds + 35 秒`；上层任务还应设置有限执行期限。

正式命令形状如下，**当前不得填写旧环境/迁移配置后试跑**：

```powershell
# 只有收到本卡明确资源和操作授权后才设置这两个入口变量。
$env:PRIMIND_OS02_OPT_IN='OS-02:replay'
$env:PRIMIND_OS02_CONFIG='<批准的配置绝对路径>'
go test -mod=readonly -p 1 -tags integration ./internal/application/repository/retriever/opensearch/integration -run '^TestRealSameChunkIdempotency$' -count=1 -v -timeout=5m
# 性能入口使用 OS-02:performance 和 TestRealPerformance；限时仍须覆盖批准窗口及有界退出。
```

独立进程仅放行配置中的精确 OS 主机和获准应用主机，清空继承代理，不改变父包的 loopback TestMain 或全局配置。所有 OS 客户端共享同一轮预算；底层连接池仍按账号/客户端独立。应用查询使用自己拥有并关闭的有限连接池。

## C4：同一真实 Chunk 重放

只接受已经通过应用 C3 生成并核对的向量 Chunk。先查固定 cluster/version、实际索引设置/mapping、alias→physical、KB/doc/chunk 精确范围，读取原始 `_source`。所有正式投影字段必须存在；未知字段会阻止执行，避免 Save 替换文档时悄悄丢字段。缺向量、零向量、维度不一致、原关键词副本、别名歧义/过滤/routing 都拒绝。

向量只在内存中来自该原记录，`embedding[SourceID]` 与 `chunk_enabled[ChunkID]` 分别构造。保留启用、推荐、来源类型、标签、文本等字段，执行 `Save × 2 → singleton BatchSave × 2`。基线和每次写后共五组快照核对完整源摘要、向量 float32 little-endian SHA256、唯一 `_id`、alias/physical 和零关键词副本。

每一步使用只读版本证据区分相同内容的不同写入：先取 `realtime=true&refresh=false` 的精确 ID GET，校验身份、完整字段、`_version/_seq_no/_primary_term`；写前必须仍等于上一轮确认状态，写后 version 和 seq 必须推进、term 不回退。随后搜索显式请求 `version=true`、`seq_no_primary_term=true`，只接受与写后 GET **三项完全相等**的命中版本及正确源摘要。旧版本在有限窗口内继续等待，超时失败；超前或相互矛盾的版本立即失败。接受前再取 realtime GET，确认静止窗口没有被打破。基线也必须先达到 realtime 版本才允许写。版本推进用于证明本轮已进入搜索视图，不算业务字段变化；SDK 重试可能推进多次，不能强制恰好加一。

关键词副本同时做精确 ID realtime GET（必须404）和原有业务范围全量搜索，避免关键词索引刷新滞后掩盖错误降级。任何写失败、部分 bulk、版本不推进、错 ID、读回差异或新版本搜索不达标均停止后续写。每步报告保留写前、写后和搜索版本，GET 成功本身不代表可检索。

输出只含标识、摘要、确认状态和计量；不输出原始向量/凭据。HTTP 错误可能已经写入，取消不是回滚证明。C4 不生成 orphan Chunk，不重新上传/重解析，不调刷新。

## C9：纯索引及持续写查

配置必须引用冻结 profile 和资源预算，以及批准的、持续更新的健康观察文件。12个字段全部必填且不得为 `null`，严格校验类型、范围、run/budget 与采样时间；缺失不能解码成实测零。CPU、队列、拒绝增量和服务 p95 的合法实测零，以及 `stop=false` 可以通过；拒绝增量非零或 `stop=true` 必须停止。内存、磁盘、OS RSS/heap 必须是正整数。观察者须按预算中的 CPU/RSS/heap/disk/队列/既有服务影响停止线生成 verdict，原始采样另存，入口不替代外部观察者作阈值裁决。入口只读取这个明确授权的文件，不自行探测主机、集群节点或既有业务。Go heap 另有入口内停止线和生成数据前的保守内存估算。

加载前的无效健康输入在联网前阻止执行；运行中各写入/查询派发点重新校验，首次失效停止后续派发、取消并等待同轮在途请求退出。不是后台实时监测器；所有请求都在途时，下一次派发检查前的响应时限仍受整体 deadline 和30秒请求期限约束。取消不表示已提交写入回滚。

第一轮矩阵保持简单：至少三轮单 worker 基线、三轮只改变并发的候选（2 或 4）、三轮持续写查；各轮至少两个不同 store，同一 cluster，同一份总/写/等待预算。向量基线保持 batch=40，关键词基线保持 batch=10。每个变体的设置、维度、规模、batch 保持一致；候选必须有匹配基线。当前入口不自动搜索最优 batch。warmup 明确标记并排除于三轮计数之外。

每轮使用显式批准、之前不存在的独立物理索引和 `os02perf_<run_id>_...` 命名空间，KB/doc/SourceID/ChunkID 均为纯索引合成范围。`allow_adapter_index_creation_and_orphan_cleanup` 必须被明确授权：正式 adapter 创建索引失败时可能删除它刚创建的孤立索引；本 harness 没有另外的删除、清空或重建路径。不同轮不复用已有数据，不碰应用 KB、A/B Chunk 或旧 store。可用对象数量也要纳入审批。

每个 store 先通过正式 Save 写一个固定检索锚点，单独计时；其余使用正式 BatchSave。固定数量 worker、无缓冲任务通道，首个错误取消并等待在途退出，最多受整体 deadline/30 秒 adapter 请求期限约束。429/item 拒绝不会被 harness 自动重放。持续写查按冻结时长均匀派发新批次，故结果标记 `paced_mixed=true`，不能当饱和最大吞吐。必须达到冻结时长与最少应用查询数。

纯索引查询调用正式 keyword/vector Retrieve；混合业务查询调用现有应用 Q-KW/Q-VEC/Q-HYB API，由现有服务处理真实 query embedding、过滤和 RRF。应用 A 证据 ID 必须先由 C3 台账确认；不使用合成向量冒充语义召回。Q-QA/Q-NEG 普通会话/SSE 与引用内容验收仍按请求草稿独立执行。

写完并停止查询后，通过 `search_after` 按 chunk_id 全量枚举到空终页；要求精确 total、完整 shards、严格升序、无重复，逐条核对独立预期的 ID/字段/向量摘要。分页不是 Top-K，未完整、超出记录预算或数据发生变化即失败。静止范围是必要前提；不声称 live cursor 提供跨页快照隔离。没有强制 refresh、降低副本或缩短向量维度。

指标含实际 NDJSON 字节、逐批 confirmed/failed/unknown/not-sent、SDK attempts、共享等待/峰值/退出计数、httptrace distinct/reused、原始耗时样本、heap/GC 与健康采样。`distinct_connections_seen` **不是活动连接峰值**；真实峰值、RSS/CPU/disk、长期增长和业务影响必须结合批准的外部采样。分位数用 nearest-rank；p99 至少 100 个成功样本。吞吐分母包含队列/重试/body；分子只计完整对账通过的批量新增唯一 Chunk，单独的 Save 锚点不混算。

入口最多报告索引和检索子路径结果，整体状态仍为 `INDEX_AND_RETRIEVAL_PASS_E2_OTHER_GATES_PENDING`。应用 queue/parse/embedding/index-confirm/最终可检索/retrieval/LLM 分段耗时、Q-QA/Q-NEG、持久化重启、旧后端/Wiki 和资源趋势仍须真实证据。纯索引结果不能关闭整个 OS-02 或业务 SLA。

## 已知边界

- 现有 `IndexConfig` 的副本字段零值会投影为 1。本入口要求非零显式有效配置；实际批准方案需要 0 时先交主控协调公共 DTO，不在这里伪造配置或改设置。
- 尚未对 OpenSearch 2.19.7 运行，协议兼容、实际模型质量、持久化、吞吐和资源都未验证。实际服务返回差异应保留失败证据再修复。
- 没有自动创建应用 store/KB/文档、重启或清理。C8-A3 在 C9 后按授权精确台账收口；保留任何未知写入对象供核对。

官方协议参考：[2.19 分页](https://docs.opensearch.org/2.19/search-plugins/searching-data/paginate/)、[2.19 写入性能边界](https://docs.opensearch.org/2.19/tuning-your-cluster/performance/)。本入口保留既有刷新/副本策略，没有应用文档中的调优建议改变被测条件。

R2版本证据参考：[2.19 GET 的 realtime 与版本元数据](https://docs.opensearch.org/2.19/api-reference/document-apis/get-documents/)、[2.19 Search 的 version / seq_no_primary_term](https://docs.opensearch.org/2.19/api-reference/search/)、[2.19 Index 默认 refresh=false](https://docs.opensearch.org/2.19/api-reference/document-apis/index-document/)、[2.19 Refresh 与搜索可见性](https://docs.opensearch.org/2.19/api-reference/index-apis/refresh/)。未调用强制 refresh、未改刷新间隔。
