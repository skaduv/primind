# OpenSearch + NebulaGraph 联合测试记录

日期：2026-11-04。分支 `claude/phase2-integration`：从 `claude/ng-phase2-1b5ec0@6cb723d` 切出，合并 `claude/os-phase2-9b4d6d@b2faa5f`（合并提交 `df3cb90`）。未推送、未开 PR。

## A. 合并、编译、测试

- 合并无冲突（51 个文件）。两边各有一份内容相同的跨租户修复（OS `9ba3f7b` / NG cherry-pick `8266191`），合并后只保留一份内容。
- `go build ./...` 通过。
- 测试通过：`retriever/opensearch/...`（含 `integration`）、`retriever/nebula`、`service/retriever`、`container`、`chat_pipeline`。
- `internal/application/service`：58 个失败，与 main 同一集合（均与两个后端无关）。
- Windows 本机构建注意事项同 `nebulagraph-progress.md`（sqlite3.h 头文件、测试需 `-ldflags=-extldflags=-static`）。

## B. 真实联合链路

环境：应用 8082，`GRAPH_BACKEND=nebulagraph`（Space `primind_ng_app`），PG 库 `primind_ng`，`.env.local` 不变；OpenSearch 2.19.3 `http://127.0.0.1:9200`；模型同 NG-C（问答/抽图 `builtin-company-chat`，Embedding 4096 维）。用户 `ng-a`（租户 10000）、`ng-b`（租户 10001）。

| 项 | 结果 | 依据 |
| --- | --- | --- |
| 存储与绑定 | PASS | `POST /vector-stores`（engine `opensearch`，index_name `primind_int_app`）→ store `c167fc51…`；新建 KB `dff866d7…` 绑定该 store，开启向量+关键词+抽图。上传后自动建索引 `primind_int_app_c167fc51-e45_4096_v1` |
| 文档处理 | PASS | 上传 `qingluan-memo.md`（1 chunk）、`xuanwu.md`（3 chunk）；两篇均 `completed`、pending 0。xuanwu chunk1 的图任务首次失败，asynq 重试后成功（`9 nodes / 6 relations`） |
| chunk 写入 OpenSearch | PASS | 数据库 6 个 chunk 与该 KB 的 OpenSearch 文档 `_id` 逐条一致（6=6）；PG `embeddings` 中该 KB 为 0 条 |
| 实体/关系写入 Nebula | PASS | 该 KB 25 实体 / 17 关系 / 26 来源，含 常州驰远电气-供应-青鸾分拣机、孙一帆-任职-常州驰远电气、北辰研究所-承担-玄武项目、葛清远-领导-北辰研究所；来源引用的 4 个 chunk 全部存在于数据库与 OpenSearch |
| 普通问答双路召回 | PASS | 问题“青鸾分拣机的伺服电机由哪家公司供应，销售代表是谁？玄武项目由哪个单位承担？”：日志 `Retrieval results, engine: opensearch, retriever: keywords, count: 6`（OpenSearch 召回）与 `entity search result count: 6 nodes, 4 relations`（Nebula 图召回）同时出现。回答“常州驰远电气，销售代表孙一帆；北辰研究所承担玄武项目”，内联引用 `40dba0cb…`（qingluan）和 `49bd804c…`（xuanwu），两个 chunk 均在 OpenSearch 中；references 3 条 |
| 删除文档 | PASS | 删除 qingluan 后：OpenSearch 该文档 2→0，Nebula 该文档实体行 10→0，KB 总行数 68→42；xuanwu 的 OpenSearch 4 条与 Nebula 15 行不变 |
| 删除知识库 | PASS | 删除 KB 后：OpenSearch 该 KB 文档 0，Nebula 该 KB 0 行 |
| 对照数据不受影响 | PASS | 删除前后对比一致：Nebula 中 NG 验收 KB 43 行；OpenSearch 中 `primind_os_app_*` 86 条、`primind_os_proxy_*` 64 条 |
| 跨租户 | PASS | 租户 B 传该 KB ID 问同一问题：无检索结果、references 0，回答“未找到相关信息”，A 的内容出现 0 次 |
| 清理 | 完成 | 删除测试 store（`DELETE /vector-stores/:id` 成功）后索引仍在（文档数 0），手动删除 `primind_int_app_c167fc51-e45_4096_v1`；`primind_int_*` 索引与别名现为 0。只动了本次创建的数据 |

## 观察（未修复，供后续判断）

- 删除向量存储不会删除其已建的 OpenSearch 索引（此次索引已为空）。是否应级联删除、或保留以防误删，需要产品决定。
- 重解析/上传期间偶发单个 chunk 图任务失败，均由 asynq 重试补上，最终数据正确；失败原因未单独定位（LLM 侧耗时/输出波动可能性大）。
