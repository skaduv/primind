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

状态：未开始。

## OS-C 生命周期（AC-OS03-01~03）

状态：未开始。

## OS-D 权限（AC-OS03-04）

状态：未开始。

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

状态：未开始。业务目标值待用户确认。

## 与 NebulaGraph 会话的共享文件

- `internal/application/service/knowledgebase.go`：暂未改动。
