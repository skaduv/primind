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

状态：未开始。

## OS-F 性能（AC-OS02-07、AC-OS03-09）

状态：未开始。业务目标值待用户确认。

## 与 NebulaGraph 会话的共享文件

- `internal/application/service/knowledgebase.go`：暂未改动。
