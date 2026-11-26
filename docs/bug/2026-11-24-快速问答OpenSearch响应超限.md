# 快速问答多库检索时 OpenSearch 响应过大导致问答失败

**缺陷类型**为 Primind 后端检索稳定性。**状态**为本地已完成返回字段过滤并通过回归，待部署和生产复测。长正文的分批读取和整次问答的资源预算待加固。本文日期和日志时间均为北京时间（UTC+08:00）。

## 现象与影响

贡献空间的成员通过共享的「快速问答-中心制度」可以获得带引用的回答。同一成员使用内置「快速问答」的默认检索范围时，知识库召回可能中断，问答失败。页面报错如下。

```text
opensearch: transport error: opensearch: response exceeds byte cap
```

## 线上复现

使用 Chrome 中的同一子账号，在三个新对话里询问同一个车辆与停车场管理问题。三次请求的向量召回设置均为 10，向量阈值为 0.5，关键词阈值为 0.3，Embedding 维度为 4096。

| 项目 | 内置「快速问答」默认范围 | 共享「快速问答-中心制度」 | 内置「快速问答」显式指定同样 6 个库 |
| --- | --- | --- | --- |
| 北京时间和请求 ID | 2026-11-24 10:07:00，`TMA49aPYZ9LW` | 2026-11-24 10:10:03，`Xh6pv3LHt6v4` | 2026-11-24 10:13:47，`zyHbNZwrAZFQ` |
| 检索目标 | 默认 `all`，7 个库 | 自动带入配置里的 6 个库 | 用 `@` 指定同样 6 个库 |
| 单次底层查询候选上限 | 350，按日志参数推算 | 300，有日志记录 | 300，有日志记录 |
| 结果 | 合并召回时响应超限 | 检索返回 10 条结果，成功加载重排模型，回答引用 2 篇文档 | 检索返回 10 条结果，成功加载重排模型，回答引用 2 篇文档 |

子账号的知识库页显示 7 个共享库。与共享智能体输入框自动带入的清单相比，内置默认范围多出「科技投资部文档演示」。后端按知识库 ID 核对，两次成功请求使用相同的 6 个库，均在失败请求的 7 库范围内。前端切换智能体时带入配置的库 ID，后端按请求中指定的目标解析范围。未指定目标时，内置快速问答默认使用该成员有权检索的本空间及共享知识库。[内置默认配置](../../config/builtin_agents.yaml#L25) · [前端选择](../../frontend/src/components/Input-field.vue#L247) · [聊天请求](../../frontend/src/views/chat/index.vue#L1535) · [范围解析](../../internal/application/service/session_qa_helpers.go#L36) · [全部知识库范围](../../internal/application/service/session_knowledge_qa.go#L358)

初次排查使用的是 2026-11-24 09:38 的 12 库失败记录和同日 09:41 的 6 库成功记录。这两次运行各自独立，子账号的同题对照以上表为准。

## 原因与相关限制

| 限制 | 当前值与作用 |
| --- | --- |
| Primind 问答召回候选上限 | 每次底层向量或关键词查询最多 500 个知识片段。组内候选上限为 `min(500, max(向量召回数量 × 5, 50) × 知识库数量)`。向量召回设置为 10 时，6 库对应 300 条，7 库对应 350 条，12 库封顶 500 条。查询把该值设为 `size`，同组知识库共享这些名额。[候选数计算](../../internal/application/service/knowledgebase_search.go#L215) · [查询构造](../../internal/application/repository/retriever/opensearch/query.go#L109) |
| OpenSearch 条数限制 | `size` 控制返回条数，`from + size` 受索引的 `index.max_result_window` 限制，官方默认 10,000。向量查询的近邻参数 `k` 允许 1 到 10,000。[索引配置](https://docs.opensearch.org/latest/install-and-configure/configuring-opensearch/index-settings/) · [向量查询参数](https://docs.opensearch.org/latest/query-dsl/specialized/k-nn/index/) |
| Primind 客户端响应字节上限 | 每个 HTTP 响应体默认 **16 MiB（16,777,216 字节）**。读到超出上限的第一个字节时返回 `ErrResponseTooLarge`，仓储层将它包装为 `transport error`。[默认上限](../../internal/application/repository/retriever/opensearch/runtime.go#L29) · [读取逻辑](../../internal/application/repository/retriever/opensearch/runtime.go#L281) · [错误包装](../../internal/application/repository/retriever/opensearch/repository.go#L346) |

旧查询返回完整 `_source`，向量索引的命中会把正文、元数据和 4096 维 `embedding` 一起回传。第七个库参与后，检索范围与候选上限同时扩大。旧查询的字段冗余增加了单次响应体积。线上日志确认响应超过 16 MiB，完整响应体积及各片段所占字节待测量。[索引写入字段](../../internal/application/repository/retriever/opensearch/crud.go#L206) · [旧搜索请求](../../internal/application/repository/retriever/opensearch/retrieve.go#L123) · [OpenSearch 字段过滤](https://docs.opensearch.org/latest/api-reference/search-apis/search/#source-filtering)

本次合并召回超限后，问答直接退出，检索路径缺少按字节分批取回的处理。添加 ReRank 模型是用户观察到的复现条件，`rerank_model_id` 用于召回完成后的重排。两次六库请求均成功加载了重排序模型。[召回失败处理](../../internal/application/service/chat_pipeline/search.go#L122) · [模型设置](../../internal/application/service/session_qa_helpers.go#L294) · [流水线顺序](../../internal/application/service/session_knowledge_qa.go#L218)

## 1000 个知识库与长正文

同一 Embedding 模型、同一存储及所属空间的 1000 个库，共享单次底层查询的 500 个候选名额。满 500 条时，16 MiB 平均只能容纳每条约 32.8 KiB 的正文、元数据和 JSON 开销。字段过滤后，长正文仍可能让单次响应超限。[模型分组](../../internal/application/service/chat_pipeline/search.go#L400) · [存储分组](../../internal/application/service/knowledgebase_search_storegroup.go#L81) · [索引正文](../../internal/application/repository/retriever/opensearch/crud.go#L211) · [Embedding 输入截断](../../internal/application/service/retriever/keywords_vector_hybrid_indexer.go#L166)

本地用启用字段过滤的 Primind 客户端测试模拟 HTTP 服务。两次请求均带 1000 个知识库 ID，模拟响应都返回 500 个命中，字段中已排除原始向量。

| 每条正文 | 模拟响应体大小，含 JSON 元数据 | 客户端结果 |
| --- | --- | --- |
| 8 KiB | 4,174,519 字节，约 3.98 MiB | 成功读取 500 条 |
| 40 KiB | 20,558,519 字节，约 19.61 MiB | 触发 `ErrResponseTooLarge` |

跨多个模型、存储或所属空间时，会发出多次独立请求，16 MiB 分别作用于每个响应。当前并发限制约束同时执行的请求，问答层仍需汇总各组结果。整次问答的候选总数、累计正文大小和耗时需要单独核算。[存储组并发限制](../../internal/application/service/knowledgebase_search_fanout.go#L20) · [OpenSearch 进程预算](../../internal/application/repository/retriever/opensearch/runtime.go#L107) · [问答结果合并](../../internal/application/service/chat_pipeline/search.go#L509)

## 临时处理和修复状态

- 线上可以使用共享「快速问答-中心制度」，或在内置快速问答里用 `@` 指定相同的 6 个制度库。两种操作都通过了子账号的同题验证。[显式指定检索目标](../../internal/application/service/session_qa_helpers.go#L36)
- 本地修复在向量和关键词检索共用的 `Repository.search` 添加 `_source_includes`，只取正文和引用所需字段。`TestRetrieveFiltersLargeUnusedSourceFields` 用模拟的 4096 维命中和 1 KiB 测试上限，在修改前复现同一错误，修改后两种检索均通过。
- 实际运行 `go test ./internal/application/repository/retriever/opensearch ./internal/application/service/chat_pipeline -count=1`，两个包均通过。修复和测试保留于本地工作树 `C:/Users/11756/.codex/worktrees/c596/primind`，生产待部署。

## 后续加固与验收

1. 部署字段过滤后，从原子账号以同一问题复测内置 7 库与共享 6 库两条路径，核对召回、重排及引用。
2. 增加按字节分批的回退。响应超限时，先按原查询条件取候选索引、ID 和分数，再分批取正文、保留召回顺序并校验归属、权限和版本。超限批次拆小重试，单条过大时给出明确提示。[OpenSearch 按 ID 批量取文档](https://docs.opensearch.org/latest/api-reference/document-apis/multi-get/)
3. 为整次问答限制并发、候选量、累计正文大小和请求耗时。达到预算时明确提示部分结果或失败原因。用 1000 库同组及多组负载测试确定预算值，覆盖长正文、单条超限、请求取消及部分分组失败，并记录每次响应大小、总耗时、应用内存和最终引用情况。分批回退、整次问答预算及真实集群的 1000 库测试均待完成。

## 开发与验收 spec

见 [BF-02 OpenSearch 检索响应体积治理](../in-progress/bug-fixes/BF-02-OpenSearch检索响应体积治理.md)。
