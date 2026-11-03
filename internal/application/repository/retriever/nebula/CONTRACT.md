# NG-01A 图接口契约与 NG-01B 交接

基线：`codex/phase2-ng-01@7f810d60ca30d7ebc7ce3613d65354df6a6cb82c`。核查日期：2026-10-29（Asia/Shanghai）。
依据：总 Spec 1.10、并行规则 1.9、实施安排 1.6、派发卡 1.10、NG-01A 1.4、阶段 4 契约及 NG-01B 1.3（实际版本以下次开工复核为准）。
本文保留 NG-01A 的 E0 设计交接和后续实现记录。第 1—4 节是 NG-01A 时点的核查；第 5—9 节记录 NG-01B 本地实现与审查修复。2026-10-29 F01 已取得隔离实库的固定功能 E2 证据，见第 10 节；应用接线、真实模型、性能和真实故障验收仍未完成。

## 1. 现有行为核对表

以下路径相对仓库根。N=`internal/application/repository/retriever/neo4j/repository.go`；NT=同目录 `repository_test.go`。源码表达的意图、已有本地测试和真实库结论分开。

| 条目 | 源码函数/行 | 已确认行为 | 既有测试/缺口 | 固定检查 ID | 候选约束 | 父 AC | 证据层级 |
| --- | --- | --- | --- | --- | --- | --- | --- |
| 三方法签名 | `internal/types/interfaces/retriever_graph.go:10–16` | AddGraph(ctx, NameSpace, []*GraphData) error；DelGraph(ctx, []NameSpace) error；SearchNode(ctx, NameSpace, []string) (*GraphData,error) | 接口只读；本卡不实现接口 | 全部 | 保持签名，Close 由内部生命周期提供 | NG-01-AC01 | E0 |
| DTO/序列化 | `internal/types/extract_graph.go:18–47`；`extract_graph_test.go:12` | 节点 name/chunks/attributes；关系 node1/node2/type；ID、KnowledgeID、SourceID、TargetID 均排除 JSON/YAML；Namespace 只有 KB/doc | 既有 JSON/YAML 测试 PASS；新增特殊字符 JSON 往返 PASS | Q4/Q7–Q9 | 不复制公共 DTO，不允许模型提供内部身份 | NG-01-AC01 | E0/E1 |
| namespace 编码 | N Labels:41、Label:50；`NameSpace.Labels:51` | 非空 KB/doc 拼 label，连字符变下划线；没有 TenantID | `a-b` 与 `a_b` 标签可能碰撞；未做真实漏洞验证 | Q7–Q9/D1 | 目标用精确 KB/doc 字段与复合身份；不照搬有损编码；授权仍归上游 | NG-01-AC01/04 | E0 |
| 搜索范围 | N SearchNode:190、graphSearchCypher:325–344 | namespace label 同时施加到种子和邻居；Knowledge 空时为 KB 标签范围 | NT:18 验证两端标签；无空 KB 参数校验，非法范围不能宣称安全 | Q1/Q7–Q9 | 空 KB 明确失败；空 doc 只允许 KB 范围读，不能全图兜底 | NG-01-AC01/04 | E0/E1 |
| 同名实例 | N addGraph:78、99–101 | 合并键为 labels + name + kg；同文档同名合并，不同文档按 kg/label 区分 | NT:159 验证解码不按名称合并；写入幂等尚未实测 | Q7–Q9/R1 | 实体键为 (KB,doc,原始名称)，保留两个“主泵A”实例 | NG-01-AC01/04 | E0/E1 |
| 属性 on-create | N addGraph:78、87；[APOC 签名](https://neo4j.com/docs/apoc/current/overview/apoc.merge/apoc.merge.node/) | row.props 为 onCreateProps，onMatchProps 是空 map；重复写不会主动更新 attributes | 固定输入属性相同，不证明冲突属性策略；并发首次创建谁获胜未测 | S1/R1/C1 | 属性保存创建时值，不改为最后写覆盖；端点先被空属性建出时也须核对 | NG-01-AC01/04 | E0 |
| 来源并集 | N addGraph:79 | chunks 用 apoc.coll.union 累加 | 本地夹具分别锁定两来源/四来源；并发事务效果未测 | S1/R1/C1 | 独立来源身份，禁止读旧数组后覆盖；跨进程 C1 必须实测 | NG-01-AC01/04 | E0/E1 |
| 关系端点补建 | N addGraph:98–101 | 关系端点即使不在 graph.Node 也通过 merge 创建，创建属性为空 | 固定样例端点齐全，未覆盖缺端点真实创建 | R1；B 补充 | 同 namespace 补建；保持 GraphData 顺序和 on-create 语义，不任意跨图重排 | NG-01-AC01/04 | E0 |
| 方向、多类型、自环 | N addGraph:101、decodeGraphSearchResult:260–274 | 类型参与关系 merge；解码按真实 Start/EndElementId 恢复方向，按关系 ID 去重 | NT:108 覆盖反向/自环/双种子；多类型写入仅固定黄金检查 | Q1/Q3/Q4/R1 | 连接/供液独立；反馈逆向；自检自环；原始类型作为数据 | NG-01-AC01/04 | E0/E1 |
| 子串/大小写 | N graphSearchCypher:328 | 使用原始名称 CONTAINS，没有转小写/归一化 | 查询构造测试不执行数据库字符串运算 | Q2/Q10 | 必须任意位置匹配，不能 STARTS WITH 替代，Q10 仍为空 | NG-01-AC01/04 | E0；黄金 E1 |
| 精确优先/限额 | N constants:20–27、graphSearchCypher:330–343 | 精确→长度→名称→kg→内部 ID；最多 200 种子、2000 行，先去重双端种子再限行 | NT:18 查询文本/参数 PASS；实际执行计划未测 | Q1/Q2/Q9；B 大样 | Q1 原始 Node[0] 先验，再比较集合；邻居不固定顺序；有界回填 | NG-01-AC01/04 | E0/E1 |
| 一跳/孤立节点 | N graphSearchCypher:329、339 | 只有有关系的种子参与一跳；当前库没有来源结构边 | Q5 夹具为空，不代表数据库已执行 | Q3/Q5 | 新增来源边和内部关系顶点不能算业务 degree/邻接；Q3 不带主泵A自环 | NG-01-AC01/04 | E0；黄金 E1 |
| 检索身份/解码 | N decodeGraphSearchResult:227–280、decodeGraphNode:283–300 | 节点按 ElementId 去重；返回 KnowledgeID/chunks/attributes；端点 ID 必须连接真实返回节点 | NT:108/159/186 PASS；两库 ID 不需相等 | Q1/Q4/Q7–Q9 | 按业务键比较，同时检查实际 ID 唯一及 SourceID/TargetID/KnowledgeID 一致 | NG-01-AC01/04 | E0/E1 |
| 空结果/错误 | N decodeGraphSearchResult:228、277–280；SearchNode:193–220 | 正常空流返回非 nil GraphData；流/解码错误返回 error，不成功返回半图 | NT:186 故障样例 PASS；空流返回为源码核对，真实空查询未测 | Q5/Q6/Q10；B 错误项 | 切勿把 error 或未完整读取变成空成功；空 slice 形态不限制 | NG-01-AC01/04 | E0/E1 |
| 禁用旧分支 | N AddGraph:57、DelGraph:129、SearchNode:183 | nil driver 时 Add/Del 返回 nil，Search 返回 nil,nil | 已有行为，仅记录，不在本卡修复 | B 错误项 | 正式 Nebula 初始化失败不可使用该分支冒充成功 | NG-01-AC01/05 | E0 |
| 具体文档删除 | N DelGraph:128–169 | 遍历 namespace，关系后节点；两条内部查询均带 kg 等于 Knowledge；空 Knowledge 不是“删整个 KB” | apoc.periodic.iterate 仅取 total，代码未核对 failedBatches/errorMessages，也未显式 Consume 结果；真实错误传播/残留待核对 | D1/D2 | 精确 doc 删除两次；A2/B1 完整保留；业务与来源结构都检查，不能只测 SearchNode 为空 | NG-01-AC01/04/05 | E0；黄金 E1 |
| 批次/事务范围 | N AddGraph:61–65、addGraph:70–119 | 每个 GraphData 两组 UNWIND，图之间顺序；事务粒度为单个 graph，之前成功的 graph 不自动回滚 | 非逐节点 RPC；批次字节双限未见 | R1/C1；P1/P5 | 保持先节点后关系、属性创建次序；部分提交显式错误 | NG-01-AC01/06 | E0 |
| 上游并发/重试 | `internal/application/service/extract.go:119–120,357–387`；`internal/router/task.go:189–215` | LLM 抽取后给节点补 Chunk 再 AddGraph；任务 MaxRetry(3)、30 分钟超时；enrichment/shared 都可处理图队列 | 本卡未执行 worker/LLM；未把各 worker 池当成图存储总预算 | C1；P3/P4/P5 | AddGraph 跨调用共享许可，重试同预算；不擅改 worker 数 | NG-01-AC01/06 | E0 |

本表揭示的旧行为/风险留在报告供主控协调；本卡未修改 Neo4j 或公共文件。APOC 当前文档只帮助解释调用参数，不证明现有服务的 APOC 版本或实际行为。

固定文件的业务键是对象，名称不与自选分隔符拼接。baseline 为 9 实体、8 业务关系、8 连通实体；四次基线与两个追加分离。checks 恰为 Q1—Q10、S1、R1、C1、D1、D2，各保存完整实体/关系键、属性/来源、操作前提及禁止范围。S1 是单实体来源快照，不是搜索；R1 是重放；C1 依赖 R1，D1 依赖 C1，D2 依赖 D1。D1 显式保存两次删除和 Q1—Q6 均空的要求，D2 要求本 run 业务/来源零残留。

`first_entity_key` 只约束 Q1 原始返回首节点；`neighbor_order=unspecified` 表示 JSON 数组顺序不是后端邻居顺序。B 的 runner 须先检查原始首节点，再做集合规范化。每个实体的来源按集合比较但拒绝重复；ID 和残留检查仍需 B 实测。A 中的 `real_database_status=NOT_RUN` 是证据边界，不由 fixture 测试改为 PASS。

`connected_entity_count` 只统计该快照内列出的关系端点。S1 只保存来源，因此该字段是 0，不表示主泵A在数据库中没有业务关系。

## 2. 最多两套映射候选及取舍

两套均未实现、未实测或冻结。以下是候选设计，不是已可执行语法。

| 候选 | 最小结构与身份 | 优点与主要风险 | 决策/验证入口 |
| --- | --- | --- | --- |
| A：业务关系记录顶点（优先实验） | Entity 保存 KB/doc/原始名称/创建属性 JSON；RelationRecord 按 (KB,doc,起点,终点,原始类型) 稳定身份，固定内部 from/to 边连接两 Entity；Evidence 按 (KB,doc,实体身份,Chunk) 独立稳定身份及来源边 | 不把任意类型变 Schema；同端点多类型不挤到一个 rank。业务一跳需要经过内部关系记录，必须只还原一个逻辑关系，内部顶点/边不能返回；写放大、分页及部分状态复杂 | Q1—Q10/R1/C1/D1/D2；B 实测一跳还原、端点方向/自环和清理；P1/P6/P7 量化物理放大。它不是新增多跳业务功能 |
| B：原生通用业务边 | Entity/Evidence 同 A；固定业务 edge type，原始关系类型存属性，方向仍为起点到终点；rank 与完整关系键需有一致、可重做的映射 | 邻接直接、物理记录较少；rank 是有限整数，简单 hash64 无法保证无碰撞，固定 0 会覆盖多类型。跨客户端分配冲突与同身份重放都要证明，不能靠进程内 map | 在跨实例一致的冲突处理及失败关闭机制实证前不采用。R1/C1/双类型/反向/自环、强制碰撞用例；不能以碰撞概率低当成无风险 |

共同约束及待冻结方案：

- Entity/RelationRecord/Evidence 使用不同域标识和无歧义长度编码的元组产生稳定 VID 候选；可实验 SHA-256 文本 VID，原始元组另存用于归属校验。哈希不构成数学上的无碰撞保证；不同元组映到同身份时必须拒绝覆盖、可诊断失败，并用注入碰撞测试检查行为。VID 类型/长度在 B 核对后选，不截断原始名称/属性，不把 kb-a/doc-a1 这些逻辑 ID 强制视为 UUID。B 的 run-id 只映射 KB/doc/Chunk，不改名称与关系类型。
- 属性采用 JSON 字符串候选，保留字符串数组及特殊字符；以首次创建的值为准。不能把所有 GraphData 合并重排，改变“先由关系补空端点，后节点属性不覆盖”的既有语义。条件创建是否能抵抗并发需真实核对，固定夹具的相同属性不是冲突测试。
- 来源以每实体每 Chunk 的独立、可重放记录维护；两客户端追加不同来源不写共享数组。读取先限业务实体再回填来源，禁止 Evidence 成为种子/邻居；来源大量时按预算分页，未完成不冒充完整结果。D1 删除文档自有 Evidence；不共享来源所有权可简化误删风险。
- 子串候选：先用 KB/(可选 doc) 索引限制物理候选，再由目标版本支持且已验证的过滤路径执行原始大小写子串，稳定排序取前 200 个合格业务种子。可以实验范围内分页过滤，但不能任意取前 N 再过滤而丢失精确候选；预算不足必须显式失败或明确可识别截断，不能成功返回伪完整集合。不得跨 KB 全图扫描或偷偷引入全文检索服务。B 必须以 Q2 的中间子串和规模化计划证明可行；如需更大的预算/产品截断变更，交主控决定。
- 业务关系一跳按最早种子归属去重，再按确定次序施加 2000 行上限。保留 Q1 首节点约束；邻居内部顺序不跨后端强制相等。请求范围同时验证两端，返回真实节点 ID 与边端点 ID 对齐；零业务边的孤立节点可以存储但 Q5 不召回。
- 删除保存 run manifest 与精确 namespace；按依赖清业务关系记录/内部连边、来源结构、实体，并可重复对账。已提交批次不能虚构回滚；失败列成功/未知/未发送，返回 error。先收集在途结果，再限定重做/检查残留；不 DROP/CLEAR Space，不删除其他 run 或共享 Schema。

W/P 交接是 B 的实现要求，本卡只完成 E0 方案：

| 要求 | 实现候选与限制 | B 的必需验证 |
| --- | --- | --- |
| W-01 批次双限 | 同 KB/doc、类型和语义分组；完整编码/转义后语句字节与记录条数双限，来源物理写也计入；空集合零 RPC，单条超限明确失败；不长期跨调用攒批 | P1 空/边界/超限/中文转义/多 GraphData；R1/C1 语义不变 |
| W-02 依赖/失败 | 节点/端点确认后才发依赖边/来源；GraphData 创建属性次序不变；首错停止新派发、等待在途收敛，区分确认/未知/未发；全部批次完成才成功 | P1 顺序；P5 首批成功后失败、响应丢失与重放；B-16 |
| W-03 池生命周期 | 优先实验一个同配置（端点/身份/Space/传输）共享 ConnectionPool，独占借用 Session，固定 Space，显式 Release；SessionPool 也需外层总预算，不能以 MaxSize 代替硬限额。初始化半失败释放；唯一 owner 幂等 Close，拒绝新调用、有限排空后关闭 | P2 构建/借还计数、各错误路径；P4 在途关闭；P6 真实 SDK 会话连接是否泄漏 |
| W-04 总并发 | 一个客户端的读写删共享总许可，AddGraph 重试和 DelGraph 共用写许可，预留查询份额；等待可取消、有限超时，内部排队须同时有条数/字节边界；不为每次 AddGraph 各建 C 个 worker。NG-02 将各进程实例预算相加 | P3 多调用+重试+Search/Del 饱和后释放；P4 取消；P6 实际峰值/查询饥饿 |
| W-05 取消/重试 | 已发 SDK RPC 不接 ctx 的路径须正传输超时；ctx 取消后停止新批次，底层结束前继续占许可，不用 goroutine 提前返回伪装取消；永久错误不主动重试。优先不叠加适配器自动重试，由上层按幂等重放；SDK 内部重发仍受同一许可 | P4 等许可/等会话/在途取消；P5 有限重试与结果未知；B-17。SDK、适配器、Asynq 尝试次数分层记录 |
| W-06 来源并发 | 独立 Evidence 身份，禁止无保护读改写 chunks；属性首次创建与来源追加分开；不能仅靠进程锁 | C1 两独立进程、两独立客户端/池同时追加 05/06，等双方结束精确查四来源，重复并发仍不增重；P5/P6 |
| W-07 观测 | 批次条数/实际字节、排队/RPC/AddGraph 墙钟、在途峰值、连接/会话、错误码/重试按 run/batch 关联；业务实体/关系与来源/内部写放大分列；不输出正文/凭据 | P6 持续运行；P7 原始轮次结果、完整对账及残留清理 |

性能方案：B 先冻结 performance-profile（版本/Schema/索引/副本/硬件/网络/后台负载、数据种子、规模、属性字节及热点比例）。先单并发批量基线，再一次改变一个批次或并发参数；Spec 的 50/200 条、并发 1/2 仅可作为预算内候选，不是生产默认值。预热与统计分开，每组至少三轮空起点新 run，分别测新写、重放和热点追加；另在事前冻结时长及最少批次/数据量后做持续运行（10 分钟只是 Spec 候选，未授权本卡执行）。不得事后降低目标。

P7 记录正确且唯一的业务节点/边吞吐、AddGraph/s、批次/调用 p50/p95 及样本数、等待/重试、可查询延迟、CPU/内存/磁盘 I/O/连接/积压；纯写计时包含等待重试，生成/预热/清理独立。对账通过 manifest 分页读全量预期，不用 SearchNode 的限额掩盖漏写。P6/P7 触发资源阈值、来源丢失、越界、错误成功、超并发或无法排空即停止派发并保留证据；OS/NG 不同时压测。B 交 performance-profile.json、performance-results.csv、performance-checks.json；本卡不生成空壳性能 runner 或声称测量达标。

## 3. 需由 NG-01B 消除的版本/环境问题

固定目标仍是社区版 3.8.0 与 nebula-go/v3 v3.8.0；没有重新选型。2026-10-29 阅读下列官方版本文档/标签源码，所得为 E0，不能替代 SDK 编译、服务自报或 E2。

| 问题及官方资料 | 当前资料结论 | B 如何消除不确定性/对应项 |
| --- | --- | --- |
| [VID](https://docs.nebula-graph.io/3.8.0/1.introduction/3.vid/) | 一个 Space 统一 VID 类型，可选 FIXED_STRING(N)/INT64，VID 是实例身份 | 验证域隔离、实际长度、元组编码、特殊/超长名称及碰撞失败；Q7–Q9、AC02/04 |
| [集合](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/3.data-types/6.list/) | list/set/map 不能直接存顶点/边属性 | 核对 JSON 属性还原与 Evidence 映射；Q4/S1/C1、AC02/04 |
| [INSERT VERTEX](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/12.vertex-statements/1.insert-vertex/) | 普通写已有 VID/tag 会覆盖；IF NOT EXISTS 不更新已存在数据 | 条件创建竞争、空端点和冲突属性另测，不能声称事务等价；R1/C1/P5 |
| [INSERT EDGE](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/13.edge-statements/1.insert-edge/) | 相同边身份会覆盖；同端点同类型可用不同 rank 区分，默认 rank=0 | 候选 B 未解决跨实例 rank 冲突前不冻结；Q1/Q3/Q4/R1、AC02/04 |
| [LOOKUP](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/7.general-query-statements/5.lookup/) | WHERE 字符串运算仅支持 STARTS WITH；索引前提和重建影响可用性 | 范围内子串路径、索引就绪有限等待、EXPLAIN/PROFILE 及候选/结果/耗时；Q2/Q10/P7 |
| [DELETE VERTEX](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/12.vertex-statements/4.delete-vertex/) | 3.8 默认保留关联边；WITH EDGE 清连边，整段删除非原子 | 不能只删顶点；D1/D2 用 manifest 查所有内部/来源残留，失败可重做 |
| [Go 客户端](https://docs.nebula-graph.io/3.8.0/14.client/6.nebula-go-client/)、[ConnectionPool v3.8.0](https://github.com/vesoft-inc/nebula-go/blob/v3.8.0/connection_pool.go) | 两种池；ConnectionPool 调用方负责 Session；createConnection 满容量返回错误，非 ctx 等待队列 | B 引入精确 SDK 并核对 thread safety、配置实际含义、初始化半失败、关闭回收；P2/P3/P6 |
| [Session v3.8.0](https://github.com/vesoft-inc/nebula-go/blob/v3.8.0/session.go)、[SessionPool v3.8.0](https://github.com/vesoft-inc/nebula-go/blob/v3.8.0/session_pool.go) | Execute 不接受 context；Session.executeWithReconnect 首次错误后会重连并再执行一次；SessionPool.executeWithRetry 也有重建/重发路径 | 列各实际路径的最大网络尝试与总时间；不要虚构关闭内部重试选项。Execute 的 Go error 为 nil 还需检查结果错误码；P4/P5 |
| [connection.go v3.8.0](https://github.com/vesoft-inc/nebula-go/blob/v3.8.0/connection.go) | 普通 TCP/TLS 分支将 timeout 交给 socket；HTTP2 创建路径不同 | 先核对选定传输的正超时、故障等待与关闭；不能泛称 context 可中断 RPC；P4/P6 |

SDK 源码所见不是已经运行的协议证据。若采用 Session 的一次初始发送+一次重连重发路径且适配器不叠加重试，Asynq MaxRetry(3) 对同一批次的图写发送候选上界是 4×2=8；这是路径限定的源码推导，不包括连接/认证/状态检查 RPC，也不保证每次都重发或延迟上界。B 必须以实际池、传输、批次数和 SDK 错误路径重新核实；SessionPool 的行为不能直接套此算式。取消后可能已写的批次标未知并对账。

| 缺项 | 谁/何种证据消除 | 影响与边界 |
| --- | --- | --- |
| 已授权隔离端点、账号安全引用、Space/Schema 创建读写权限、run 命名/清理边界 | 主控登记资源；B 读取服务自报版本和权限、执行限定试写 | B 的真实 CRUD/AC03/04/05 阻塞；不阻塞 A 本地完成，也不阻塞 B 已选版本本地开发 |
| 主机 CPU/内存/磁盘余量、总连接/并发/数据量/时长/排空及停止阈值、OS/NG 错峰窗口 | 资源负责人/主控冻结；B 按预算选择有限工程参数并保存 profile | P6/P7、父 AC06 阻塞。没有预算不能自行压测；本地 GOMAXPROCS=2/-p 1 不是服务负载授权 |
| 代表业务规模、导入时限、允许错误/重试、积压/排空、现有 Primind/模型延迟影响上限 | 业务负责人提供目标，B 提供基线给 NG-02；NG-02 冻结 | 不阻塞 NG-01 技术方案；业务性能达标保持 BLOCKED，不能虚构 SLA |
| SDK/Go 组合、目标 Schema/索引就绪、跨实例来源和属性冲突、查询计划、删除残留 | B 必要依赖编译+P1–P5、本地向量，再 C1/Q2/D1/D2/P6/P7 E2 | 本卡不引 SDK；Go 1.27 本地通过不能声称 Go 1.26 CI 已通过 |
| 旧 Neo4j 边界问题与后续对照资源 | 主控决定是否另派范围；B 仍保持阶段契约，后续 NG-04 做双端 E2 | A 不修公共代码；不复制 nil-driver 成功或有损 namespace 编码，不静默改变原生命周期 |

## 4. 本地命令及证据索引

执行目录仅为 `D:/work-tree/work-tree/10-项目/03-公司项目/primind-nebulagraph`。当前 Go 为 1.27.0 windows/amd64，go.mod 仍为 1.26.0。每条测试命令在独立 PowerShell 进程设置 `GOMAXPROCS=2`、清空 `PRIMIND_NEO4J_TEST_URI`，串行运行；检查了所选测试及相关 init，未加载私有配置。真实 TestSearchNodeIntegration 被精确选择器排除，不把其未运行写成 SKIP/PASS。

```powershell
$env:GOMAXPROCS='2'
$env:PRIMIND_NEO4J_TEST_URI=''
go test -mod=readonly -p 1 ./internal/types -run '^TestGraphRetrievalIdentitiesDoNotChangeExtractionSerialization$' -count=1 -v -timeout=2m
go test -mod=readonly -p 1 ./internal/application/repository/retriever/neo4j -run '^Test(GraphSearchCypherBoundsAndOrders|DecodeGraphSearch.*)$' -count=1 -v -timeout=2m
go test -mod=readonly -p 1 ./internal/application/repository/retriever/nebula -run '^TestContractFixture' -count=1 -v -timeout=2m
```

三条均实际运行、退出 0。日志在 `outputs/phase2-ai/NG-01A/2026-10-29-01/`：dto-test.log、neo4j-local-test.log、fixture-test.log。前两条共 5 个顶层测试；第三条 3 个顶层测试，初次包含 15 个黄金检查和 18 个破坏样例子测试。黄金测试不创建数据库结果，测试日志逐项注明 E1 integrity / real DB NOT_RUN。

单独复核发现空结果缺少 entity_keys 会被当作空集合接受，补第 19 个破坏用例先复现失败（fixture-review-red.log，退出 1），再为黄金格式显式要求 entity_keys/relation_keys/nodes 集合，重跑全部夹具测试通过（fixture-review-green.log，退出 0；此日志对应最终代码）。修复不改变运行时 GraphData 的 nil/空 slice 契约；还加入集合重排仍可通过的检查，Q1 原始首节点元数据保留。复验进程额外清空 LOG_PATH、关闭 LLM_DEBUG_LOG；首次进程这两个日志变量经只读检查均未设置，未产生范围外日志。

`contract_fixture_test.go` 严格解析未知字段/尾随 JSON，使用现有 NameSpace/GraphData；人工表固定每个查询的完整集合，输入计数只校验引用闭合，不生成查询预期。破坏样例验证错误来源、反向边、丢类型、特殊字符、Q1 首节点约束、非 nil 空图、删除误伤及伪造数据库 PASS 会失败。JSON 往返验证特殊字符串及检索身份不进入抽取序列化；既有测试另覆盖 YAML。没有适配器运行、真实并发、数据库取消或删除残留的 E2 证据。

验收/审查及结束提交见同目录运行报告 REPORT.md。NG-01A 的 E0/E1 完成不等于 NG-01 父任务完成；W/P 均为待由 B 实现/运行的交接项，不提前标机制或性能 PASS。

## 5. NG-01B 实现候选（2026-10-29，E2 尚未冻结）

本节承接已接收 A 的前四节；前文“B 待实现”“不引 SDK”描述的是 A 交付时点。当前固定依赖为 nebula-go/v3 **v3.8.0** 与其 fbthrift 必要依赖。A 的两个 JSON 及 contract_fixture_test.go 保持不变。当前只有 E0/E1 证据，不能将本节候选写成已验证的真实映射，也不能释放 NG-02。

| 目标问题 | 一手依据 / 查得的限制 | 当前实现与证据边界 |
| --- | --- | --- |
| 服务与 SDK / Go | [Go SDK 3.8](https://docs.nebula-graph.io/3.8.0/14.client/6.nebula-go-client/)、[v3.8.0 生成客户端](https://github.com/vesoft-inc/nebula-go/blob/v3.8.0/nebula/graph/graphservice.go) | 进程级 Go 1.26.0 编译；依赖固定。InspectService 要求 GRAPH/META/STORAGE 自报 3.8.0；实际版本 NOT_RUN |
| 身份、字符串 | [字符串与 FIXED_STRING](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/3.data-types/3.string/)、[DESCRIBE SPACE](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/9.space-statements/4.describe-space/) | 使用 FIXED_STRING(1024) VID、STRING 属性；超长返回错误，不截断；独立 UTF-8 编码向量及 S/T 本地通过，实库待验 |
| Tag/Edge/Index | [CREATE INDEX](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/14.native-index-statements/1.create-native-index/)、[SHOW CREATE INDEX](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/14.native-index-statements/2.1.show-create-index/) | 三 Tag、三内部 Edge，各有 kb/doc 索引。核对字段/非空/TTL/VID/索引定义；定义可见后才建索引，有限等待。既有不兼容结构报错 |
| 集合及来源 | [List 不能用作属性类型](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/3.data-types/6.list/)、[INSERT VERTEX](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/12.vertex-statements/1.insert-vertex/) | attrs 为 JSON 字符串；来源按 entity+Chunk 独立记录，无读旧数组再覆盖。属性 IF NOT EXISTS；首次并发创建冲突、C1 均须 E2 核实 |
| 关系身份 / 方向 | [INSERT EDGE](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/13.edge-statements/1.insert-edge/) | 完整有向业务关系变为稳定关系顶点，内部 from/to 边 rank=0；不以 type 的哈希 rank 区分关系；多类型、反馈、自环本地黄金比较 |
| 子串与查询计划 | [LOOKUP 限制](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/7.general-query-statements/5.lookup/) | KB/doc 索引限定候选，解码后 Go strings.Contains 做大小写敏感子串；不以 STARTS WITH 替代。超过快照预算报错；PROFILE 与服务耗时 NOT_RUN |
| 删除 / 孤边 | [DELETE VERTEX](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/12.vertex-statements/4.delete-vertex/)、[FETCH](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/7.general-query-statements/4.fetch/) | 内部边也存 namespace 并有索引，先精确 DELETE EDGE 再 DELETE VERTEX WITH EDGE；可在顶点已消失时定位孤边。非事务，失败必须重放/对账；manifest 另按显式 VID/边核验 |
| 池、重连和取消 | [Session.reConnect](https://github.com/vesoft-inc/nebula-go/blob/v3.8.0/session.go)、[ConnectionPool.deactivate/Close](https://github.com/vesoft-inc/nebula-go/blob/v3.8.0/connection_pool.go)、[SDK 传输构造](https://github.com/vesoft-inc/nebula-go/blob/v3.8.0/connection.go) | 源码中 reConnect 将旧连接以 false/false 从池移除，此路径未显式关闭；不能据 MaxConnPoolSize 证明物理连接硬上界。最终不调用该重连路径，改用同版本公开 GraphServiceClient + 自有连接所有权 |
| 真实协议层 | [fbthrift Socket](https://github.com/vesoft-inc/fbthrift/blob/fa2f34755b28/thrift/lib/go/thrift/socket.go) | TCP/TLS、128 KiB buffered + HeaderTransport/HeaderProtocol，与 SDK TCP 构造一致；自有 net.Conn 的绝对 deadline 限制每次读写刷新。net.Pipe 真实 Thrift 帧覆盖握手、认证失败、USE、执行、部分成功及 signout；无监听端口，不是 Nebula 实库验收 |

### 5.1 Schema 与身份

SchemaDDL 返回可审查的 12 条非破坏性 DDL：三 Tag、三 Edge、六个 namespace 索引。Space 必须由资源登记预建。InitializeSchema 仅在显式授权且 SHOW TAGS/EDGES 为空的专用 Space 创建结构；不 CREATE/DROP/CLEAR Space，不 ALTER 已有结构，不自动为既有数据 REBUILD 索引。半途失败保留现场，重新运行前由资源负责人核实结构。

- primind_entity：kb、doc、name、attrs，均 STRING NOT NULL。
- primind_relation：kb、doc、node1、node2、kind，均 STRING NOT NULL。
- primind_source：kb、doc、name、chunk，均 STRING NOT NULL。
- primind_from：关系记录 → 源实体；primind_to：关系记录 → 目标实体；primind_owner：来源记录 → 实体。三者均含 kb/doc STRING NOT NULL，rank=0；不是业务关系类型。
- 每个 Tag/Edge 都有对应 *_scope 索引，字段 kb(128), doc(128)；TTL 必须关闭。

VID 为 ng1_ + base64url(JSON 字符串数组)，数组为 [role, KB, doc, ...]。entity 追加 name；relation 追加 node1/node2/type；source 追加 name/chunk。编码可逆，不截断、不依赖哈希碰撞概率。KB/doc 按完整原值区分，禁止标签化替换字符；单个 KB/doc 限 96 UTF-8 字节（base64url 不超过索引 128 字节）。所有用户属性使用规范 base64url，attrs 内部先 JSON 编码，因此引号、反斜杠、真实换行、NUL 均不会进入 nGQL 语法。读取时严格解码并复核 ID、namespace、关系方向和 rank。

VIDBytes=1024 是容纳可逆组合身份的工程起点，不是已测最优值。[官方 VID 文档](https://docs.nebula-graph.io/3.8.0/1.introduction/3.vid/) 提醒过大的 FIXED_STRING 会增加内存和磁盘开销、影响性能；本方案须把较长 VID 的物理放大计入 P6/P7，不能以无哈希碰撞推导性能更优。目标 Space 仍须真实核验。

先实体，再来源/关系记录，再内部边；多个 GraphData 顺序处理，实体 IF NOT EXISTS 保留 on-create 属性。来源每个 Chunk 一条稳定记录，重复输入幂等。没有跨批事务；关系或来源缺少内部连边时 Snapshot/Search 返回错误，不能把半图当完整图。首次创建时的属性竞争、写入可见性、跨进程来源完整性尚未真实验证。

### 5.2 查询与删除语义

SearchNode 实现现有接口；KB 必填，doc 可空，未知但有效 KB 允许无命中。返回非 nil 空 GraphData；孤立节点不成为检索命中。候选以精确名优先，然后 Unicode 字符数、名称、doc、ID 排序；最多 200 个 seed，按最早 seed 分配关系并去重，再最多 2000 条业务关系。返回 ID/KnowledgeID/SourceID/TargetID，首个节点是首条命中的 seed。邻居顺序不作为业务契约。

Search 提供额外 SearchDiagnostics（范围节点/关系数、候选/选中 seed、返回关系数及两个截断标志）；原 SearchNode 不改 DTO，截断累计在 Metrics.SearchTruncations。Snapshot 走完整限定范围检查，不受 200/2000 显示上限影响；缺内部边、额外孤边、错误来源、跨 namespace、重复身份、解码错误和预算溢出均返回 error，不返回部分成功。

范围快照不是数据库事务快照。与正在写入/删除的数据并行时可以报不完整；调用方不得据此宣称原子发布。LOOKUP 的 LIMIT 限制返回量，不等于已经证明后端扫描成本有界；实际索引计划、超级节点开销及真实吞吐仍需 PROFILE/P6/P7。ReadBytes 是解码检查预算，不是 Thrift 在分配响应之前的网络帧/RSS 硬限制。

DelGraph 拒绝空 KB 或空 doc，避免误作整 KB 删除。所有 namespace 先校验和读取、完整构造删除计划，再执行内部边删除、带边顶点删除。内部边 namespace 索引使顶点已删后的孤边也能被再次定位；无全库兜底。ProbeManifest 按预先记录的所有顶点/边身份 FETCH，配合 namespace 快照验证保留对象与无残留；不能仅以普通问答为空判定删除成功。图内写删竞争、索引可见性、真实故障恢复仍未验收。

## 6. NG-01B 执行预算与生命周期（最终本地实现）

| Config 字段 / 单位 | 默认值 | 校验上界 / 说明 |
| --- | --- | --- |
| Connections / Writes / Waiters | 4 / 2 / 32 | 连接 2—64，1 ≤ 写 < 连接；等待者 1—1024，计划等待与执行等待合计受限；至少一个读许可 |
| BatchRecords / 条 | 100 | 1—10000；按同 namespace、结构、语义拆分，空集合不发请求 |
| StatementBytes / byte | 64 KiB | 256 byte—4 MiB；完整编码语句（含前后缀/逗号/属性/VID）计数 |
| OperationBytes / byte | 8 MiB | ≥ StatementBytes，≤64 MiB；整个写删计划上限，单条超限明确失败 |
| PlanBytes / byte | 32 MiB | ≥ OperationBytes，≤256 MiB；每个非空写删操作先预留 OperationBytes，约束同时构建/持有的编码计划；不是进程 RSS 限额 |
| PendingBytes / byte | 8 MiB | ≥ StatementBytes，≤64 MiB；等待执行的完整语句字节预算 |
| SnapshotRecords / 条 | 10000 | 1—100000；快照顶点+内部边超过预算报错；输入准备亦有记录数预算 |
| ReadBytes / byte | 16 MiB | 256 byte—128 MiB；SDK 响应与各解码阶段检查，不能取代服务端响应限制 |
| VIDBytes / byte | 1024 | 128—4096，必须与实际 Space 的 FIXED_STRING 长度完全一致 |
| AcquireTimeout / duration | 5 s | 正值，≤5 min；许可和计划内存等待可取消 |
| SocketTimeout / duration | 5 s | 正值，≤5 min；每个真实 RPC 的绝对 deadline；TCP/TLS 拨号也受限 |
| OperationTimeout / duration | 30 s | 正值，≤5 min；跨批总 deadline，不因逐批成功而刷新 |
| CloseTimeout / duration | 30 s | 正值，≤5 min，且 ≥2×SocketTimeout；处置在途 RPC 与并行 signout |
| ReadyTimeout / PollInterval | 20 s / 100 ms | 正值，≤5 min，poll≤ready；由实现选取的工程初值，不是生产 SLA |

同进程、完全相同配置的 Repository 句柄共享一个注册客户端及固定上限的 session 槽。配置摘要包括账号/密码/TLS 配置，仅在内存中用于复用键，不打印。连接懒创建，初始化验证一个连接；每个 session 固定账号/Space，借还期间独占。后续相同配置新句柄不会再建池，但会只读核对 Schema。每个句柄一次释放引用，最后一个 Close 停止新工作、等待计划与在途请求，最终关闭所有自有连接。初始化半失败会关闭已经取得的连接；关闭超时返回错误，不假称资源全部释放。

生产传输使用固定 SDK 的公开生成客户端与 GenResultSet，不使用 Session/SessionPool 自动重连。SDK 自动重发=0，适配器重试=0，单个业务批次网络执行最多一次；新连接的 verify/authenticate/USE 是独立初始化 RPC，不混算为业务批次。沿用上层 Asynq MaxRetry(3) 时最多 4 次上层执行，每次重放仍经过同一许可；未在本卡修改或接入 Asynq。不同进程预算相加，C1 运行需登记主进程+两个 worker 的最坏总连接预算。NG-02 再结合实例数和应用 worker 配额。

取消会关闭实际 net.Conn 并等待 SDK 调用退出，之后才释放执行许可；没有“后台 RPC 继续、前台先成功”的行为。传输错误/取消后丢弃该连接，不在可能错位的响应流上重试。新业务调用可懒建替代连接，但先前结果仍是未知，须重放/对账。中途失败的 OperationError 包含每批 statement digest、脱敏身份摘要及 confirmed / unknown / not_sent；服务端失败、部分成功均非 nil error，保守将已发送失败列为 unknown，无自动回滚。

Metric 仅记结构种类、条数、编码字节、等待/驱动调用耗时、返回行数、错误码与语句摘要；不存查询原文/凭证。StatementCalls 是驱动调用数，不是所有物理网络 RPC 数；RPCNS 包括当次懒建连初始化时间。SDKReconnects 在本实现恒为 0。AddCalls/AddWallNS、读写峰值、排队及计划字节、截断计数可读取。最多保留 10000 个明细样本，溢出显式计数；TakeSamples 由单个测量消费者分段取走，真实性能入口遇样本丢失即失败。

## 7. NG-01B 本地验证与真实运行入口

本地测试包含：固定编码向量、完整语句双限/依赖、多 GraphData、共享池/半初始化失败、等待与在途取消、真实 net.Pipe 绝对 deadline、真实 SDK Thrift 帧、服务端部分成功、读许可/删限流、整体计划内存预算、200/2000 截断、孤边删除、Schema 不兼容及异步就绪、A 独立黄金 Q1—Q10。协议基线来自 A 固定 baseline，业务预期来自固定 checks；不由 AddGraph 输出或待测遍历算法生成答案。协议通过不能证明真实持久化或 C1。

真实入口默认安全跳过，SKIP 在报告中必须记 BLOCKED/NOT_RUN。启用前由主控提供隔离资源、Schema/清理授权、连接/负载预算与窗口，并填充独立 profile。loadRun 不读取私有 .env；profile 源码 SHA 必须匹配当前 HEAD。输出目录必须绝对且本轮文件不存在，拒绝覆盖旧证据。

必需的进程环境：PRIMIND_NG_E2_ENABLE=authorized-isolated-run；PRIMIND_NG_PROFILE 指向已授权 profile；PRIMIND_NG_OUTPUT 指向新的证据目录；PRIMIND_NG_ADDRESSES、PRIMIND_NG_USERNAME、PRIMIND_NG_PASSWORD 由安全会话注入。TLS 可设置 PRIMIND_NG_TLS=true、PRIMIND_NG_SERVER_NAME、PRIMIND_NG_CA_FILE。不得将密码写进 profile、报告或命令历史。本次没有设置真实启用值。

```powershell
# 从登记的 Nebula worktree 执行；连接与授权资料必须事先补齐。
$env:GOMAXPROCS='2'
$env:GOTOOLCHAIN='go1.26.0'
$env:PRIMIND_NEO4J_TEST_URI=''
$env:LOG_PATH=''
$env:LLM_DEBUG_LOG='false'
go test -mod=readonly -p 1 ./internal/application/repository/retriever/nebula -run '^TestNebulaRealContract$' -count=1 -v -timeout=30m
# 性能另需 PRIMIND_NG_PERF_ENABLE=authorized-load-profile，必须与 OS 错峰。
go test -mod=readonly -p 1 ./internal/application/repository/retriever/nebula -run '^TestNebulaPerformance$' -count=1 -v -timeout=130m
```

TestNebulaRealContract 在写入前生成 run-manifest.json，覆盖四个固定调用和两个补充来源；run 前缀仅用于 KB/doc/Chunk，不改变 S/T/name/type。先以正式接口完成 B-11 的 A1 两次写入、Q3/Q4、重复删除和范围/manifest 零残留，再重建完整固定输入并完成 Q1—Q10、未知 KB、重放前 S1、R1、至少三轮 C1、D1/D2。可见性轮询的有限 ctx 直接传到每次实际探测。每轮 C1 恢复 A1 原始两来源状态、启动两个独立进程/连接集合，认证就绪后同一屏障放行，再独立黄金对账。失败保留现场；成功清理后以 manifest 显式核对内部边和顶点。这个流程目前没有运行。

TestNebulaPerformance 使用固定 seed 的合成 GraphData；每个 group pair 固定两实体、四条有向关系、三条来源，保持特殊字符和同名不同文档。每档预热独立，正式≥3轮，各轮新 run；分别测新写/重放/热点来源追加，另测选定档持续运行。group_pairs 可让单图记录数跨过 50/200 批次候选，不能将小于两个档位的相同负载解释为批次收益。独立逻辑预期先落盘，纯写模式墙钟包含正式 AddGraph、调度等待、资源采样和失败耗时，对账/可查询等待另计；单次调用分位数仅计 AddGraph 调用。持续运行窗口另含并行读探测，不与纯写吞吐混算。每250ms尝试资源采样，持续写时同时探测查询；真实时间序列仍需复核。新增实体/关系/来源吞吐根据独立预期和读回对账计算，重放新增为0；confirmed_*只代表服务器确认的记录尝试，不代表新建记录。按 namespace 做不受搜索显示上限影响的对账，再限定删除并查 manifest 残留。

profile 必须冻结资源/数据量/并发/时长/最小批次与调用数/持续运行最大调用数/停止阈值。资源负责人持续更新 resource_sample_path 指向的本地 JSON，包含时间、CPU、内存、磁盘读写速率/余量、连接/session、既有服务延迟及错误率；超过 10 秒或字段缺失即停止。不会擅自在共用主机装采集器、改配置或停服务。真实资源趋势与跨进程 C1 证据仍需主控复核，入口输出 MEASURED 不自动把 P6/P7 或业务 SLA 标成 PASS。

当前报告目录三份性能文件是 **未运行模板/状态文件**，CSV 只有表头，没有虚构测量行。没有实库数据需要清理，没有服务/Schema 创建或重启记录。

## 8. 当前门槛与后续输入

NG-01A 已接收不等于 NG-01B、NG-01 或真实图数据库验收通过。本卡状态为“部分完成/受阻”；可审查本地实现，不标整卡完成/待审查，不自动进入 NG-02。

待主控登记：指定端点与安全凭据引用；社区 3.8.0 实例；专用 Space/VID/Schema 与创建权限；run 清理边界与失败现场保留规则；C1 三进程总连接预算；性能窗口、资源采样人/路径及负载/停止/排空预算。业务 SLA 单列，不能以技术机制或测量完成替代。实库前须复核当前生成语句、Schema 结果列、TLS、索引与删除语义；发现 E2 不符继续在本卡修复，不改黄金预期。

## 9. NG-R1 / NG-R2 审查返工（2026-10-29）

NG-R1：执行结果为 E_SESSION_INVALID(-1002) 或 E_SESSION_TIMEOUT(-1003) 时，淘汰该会话并关闭本实现拥有的物理连接，然后归还空槽。**当次失败继续返回原错误和 unknown 回执，不重新认证或重放当次语句**；下一次独立调用才在原连接/写许可内懒建并认证。语法、权限、partial 等普通服务端失败仍返回错误并归还原会话，不笼统重连。SDK/适配器自动重试仍为0，外层预算不变。

依据：[3.8.0共同错误码定义](https://github.com/vesoft-inc/nebula/blob/v3.8.0/src/interface/common.thrift#L322-L332) 将两者列为会话错误；[GraphService执行入口](https://github.com/vesoft-inc/nebula/blob/v3.8.0/src/graph/service/GraphService.cpp#L141-L163) 对缺失/无效会话返回 INVALID；[SDK SessionPool](https://github.com/vesoft-inc/nebula-go/blob/v3.8.0/session_pool.go#L479-L508) 对 INVALID 先关闭再尝试新会话。本实现只参考其淘汰原则，不引入其自动重放；未观察目标实库触发 TIMEOUT 的实际路径。net.Pipe 真实 Thrift 帧验证两码的旧连接关闭、新连接认证、共享池上限、并发调用、取消/关闭和普通错误保留；仍只有E1证据。

NG-R2：性能profile的 Concurrency 必须以1为首档、每档唯一、至少含一个大于1且不超过 MaxConcurrency 的档位。重复串行、重复并行、末档回到1等退化矩阵直接拒绝。矩阵仍按登记顺序完整执行；持续运行继续选择最后一个 batch/concurrency 组合，因此所选组合已在正式矩阵内，且并发大于1。允许 `[1,3,2]` 这种明确选择较低并发为持续档的顺序，不擅自排序改变选定档。

本次返工证据为 `outputs/phase2-ai/NG-01B/2026-10-29-02/REPORT.md`。首轮报告/日志及三份未运行性能文件原样保留；R1/R2本地修复待主控复审，整卡仍“部分完成/受阻”，E2/C1/P6/P7仍BLOCKED/NOT_RUN，不进入NG-02。

## 10. F01 固定功能实测（2026-10-29）

本节更新前文相应的“尚未实测”状态，限本轮已覆盖行为。代码提交 `25ad2f0cf0e5e8f6b5bb26b1ede91f5af23321a1`，报告与原始证据位于 `outputs/phase2-ai/NG-01B/2026-10-29-functional-01/`。F01 功能证据待主控审查；NG-01B 和父 NG-01 仍因 P6/P7 等必需证据未取得而受阻，不释放 NG-02。

- 通过既有 SSH 隧道 `127.0.0.1:19669` 使用专用 `primind_ng_contract`；GRAPH/META/STORAGE 均回报 3.8.0、ONLINE。Space 为 10 分片、1 副本、FIXED_STRING(1024)、utf8/utf8_bin。运行前无 Tag/Edge/索引，首轮创建后保留三 Tag、三 Edge、六个 kb/doc 索引和关闭 TTL 的定义；未清空或删除 Space。
- 完整 `TestNebulaRealContract` 在 `contract-01`、修复后的 `contract-02` 各通过一次；最终 run 为 `ng01b_f01_20261029_a4`，使用既有已核实 Schema。每次 18 项检查、21 次独立预期对照全部通过；Q1—Q10、S1/R1、三轮 C1、D1/D2、未知有效 KB、B-11 和 manifest 无残留均有原始证据。每轮 C1 有两个不同 PID 及各自通过日志。固定输入、黄金预期和本地参考模型的 SHA256 均未变。
- 基线实体/业务关系为 9/8；S1/R1 保留两来源，C1 三轮保留预期四来源；D1 保留其他 namespace 的 4 实体/2关系，D2 归零。每次 manifest 预分配 29 顶点、28 内部边；限定查询与精确身份 FETCH 最终均无残留。6 份范围查询计划含 TagIndexPrefixScan/EdgeIndexPrefixScan；它们不构成大数据扫描成本或性能达标证据。
- 实测发现并修复两项结果解码兼容性：已知 DESCRIBE 元数据形状的可选 Comment/Default 列可接收 SDK `empty`；SHOW TAG/EDGE INDEXES 的 Columns 列接收仅字符串列表并编码为有界 JSON。其他表形状、必需元数据 EMPTY、非字符串/嵌套列表仍报错，不把未知类型吞为空串。保留修复前失败日志和定向红/绿测试。
- Connections=4、Writes=2、C1 总连接预算=12、StatementBytes=65536、实例数=1、C1=3轮、测试总业务时间上限600秒、登记排空60秒；未提高旧预算。主进程计数器最终 active/active_writes/waiting/plan_reserved_bytes 为零；这些不是所有进程物理连接与服务端 session 的测量。
- 普通正式测试命令：`go test -mod=readonly -p 1 ./internal/application/repository/retriever/nebula -run '^TestNebulaRealContract$' -count=1 -v -timeout=12m`。实际使用已缓存 Go1.26.0 可执行文件、进程级 GOTOOLCHAIN=go1.26.0/GOMAXPROCS=2/GOPROXY=off，凭据仅通过授权部署回执注入进程环境。执行命令、退出码、profile 与每次 SHA 见各 `command-result.json`。不得直接重用已完成 run_id 或把 create_empty_schema 改回 true 后盲跑。
- 本轮为隧道内明文 Thrift，未验证 Nebula 原生 TLS、账号最小权限、真实取消/断连/重启、P6 持续运行和 P7 性能基线，也没有真实模型/业务服务/应用工厂接入。固定样例成功不扩大到这些边界。

元数据依据：[3.8.0 DESCRIBE SPACE](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/9.space-statements/4.describe-space/)、[SHOW TAG/EDGE INDEXES](https://docs.nebula-graph.io/3.8.0/3.ngql-guide/14.native-index-statements/2.show-native-indexes/) 和缓存 SDK v3.8.0 的 `ValueWrapper.GetType/IsEmpty/AsList`；服务端实际列类型以本轮失败/复验日志为准。
