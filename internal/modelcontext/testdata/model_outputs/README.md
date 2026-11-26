# 模型原始输出回归样本

每个 `*.sample.md` 存一段模型真实写出的回答（脱敏、逐字），以及它应当展开成的结果。`TestModelOutputCorpus` 遍历本目录逐个断言，任何一个样本展开不对都会让测试失败。

这是 BF-01（[引用标记变体兼容](../../../../docs/in-progress/bug-fixes/BF-01-引用标记变体兼容.md) 的 4.5 节）建立的样本库：接入新模型、或者日志里出现新的 `variant` 时，把那段输出加进来。

## 新增一个样本

1. 从消息表或日志里取出模型的原始输出，去掉制度原文以外的个人信息和内部编号。
2. 新建 `NNN-模型名-变体简述.sample.md`，头部逐项写清：
   - `model`：写出这段回答的聊天模型
   - `date`：首次出现的日期（北京时间）
   - `source`：出处在哪（知识库、问题、路径）
   - `citable`：这次回答里可以引用的句柄，如 `c1-c22` 或 `c1, w1`
   - `variants`：期望统计到的变体计数，如 `square_double_quoted=4`；没有变体就留空
3. `## Raw model output` 下用 ```text 代码块放原文，`## Expected expansion` 下放期望的展开结果。

## 展开结果的写法

- `cN` 展开为 `<kb doc="Doc N" chunk_id="chunk-N" />`，`wN` 展开为 `<web url="https://example.com/N" title="Example N" />`；未登记的句柄和 `dN`、`bN` 被删除。
- 期望结果里不应再出现 `ref id=`。如果某种新写法没有被规则表覆盖，原始标记会留在展开结果里，测试随之失败：这时在 `internal/modelcontext/citations.go` 的 `citationVariants` 表里加一行，写明首次出现的模型和日期。

## 已有样本

| 文件 | 模型 | 日期 | 变体 |
| --- | --- | --- | --- |
| `001-qwen3.8-27b-bf16-square-bracket.sample.md` | qwen3.8-27b-bf16 | 2026-11-23 | `square_double_quoted` ×4 |
