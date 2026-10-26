# Primind

Primind 是内部使用的知识库与 RAG 平台：把团队的文档集中起来，提供可检索、可推理、可持续更新的问答与知识管理能力。平台支持文档解析、向量检索、RAG 问答、Agent 与 Wiki 知识图谱等能力，仅在内部环境部署使用。

## 主要能力

- **知识库与文档解析**：支持 PDF、Office、Markdown、网页等格式；内置解析引擎，可接入 MinerU、PaddleOCR-VL、OpenDataLoader 等外部解析服务。
- **检索与 RAG**：混合检索（向量 + 关键词），支持 PostgreSQL(pgvector)、Qdrant、Milvus、Weaviate、Elasticsearch/OpenSearch、腾讯云 VectorDB 等多种向量库。
- **模型接入**：内置多家厂商（OpenAI、Anthropic、Gemini、DeepSeek、通义千问、智谱、腾讯云 LKEAP、火山引擎等），支持自建/本地模型（Ollama、vLLM 等）。
- **Agent 与工具**：多步推理、联网搜索、MCP 工具、技能（Skills）与沙箱执行。
- **Wiki 知识图谱**：从文档自动生成互链页面与关系图谱。
- **多端访问**：Web 界面、命令行工具（cli/）、Go SDK（client/）。

## 架构

- `cmd/server`：Go 主服务（HTTP API + 任务队列 + 检索/问答流水线）。
- `internal/`：核心业务代码（application、handler、router、models、infrastructure 等）。
- `frontend/`：Vue 3 + TypeScript 前端（Vite 构建）。
- `docreader/`：文档解析服务（Python，gRPC），供主服务调用。
- `cli/`：命令行工具（Go）。
- `client/`：Go SDK。
- `mcp-server/`：MCP 服务（Python，已标记为弃用，内置 MCP 端点为主推方式）。
- `docker/`、`docker-compose.yml`：容器构建与本地编排。
- `helm/`：Kubernetes 部署 Chart。

数据流：上传文档 → docreader/内置引擎解析 → 分块与向量化 → 向量库/全文检索 → RAG 问答或 Agent 推理。

## 本地开发

代码提交、AI 协作、PR 审查与合并流程见 [代码提交与协作规范](CONTRIBUTING.md)；GitHub 上的门禁启用步骤见 [CI 门禁启用说明](docs/CI_GATE_SETUP.md)。

前置依赖：Docker、Go 1.26+、Node.js 20+。

1. 准备配置：

   ```bash
   cp .env.example .env
   ```

   按需修改 `.env` 中的模型与存储配置。

2. 启动基础设施（Postgres、Redis、MinIO 等）：

   ```bash
   make dev-start
   ```

3. 启动后端：

   ```bash
   make dev-app
   ```

4. 启动前端：

   ```bash
   make dev-frontend
   ```

浏览器打开前端开发地址即可。也可以直接用 Docker 启动整套服务：

```bash
docker compose up -d
```

打开 `http://localhost` 进入界面，按引导完成初始化（创建账号、配置模型、创建知识库）。

## 构建镜像

从源码构建全部镜像：

```bash
make build-images
```

该命令等价于 `./scripts/build_images.sh`，构建以下镜像：

- `ghcr.io/skaduv/primind-app`：主服务镜像
- `ghcr.io/skaduv/primind-docreader`：文档解析服务镜像
- `ghcr.io/skaduv/primind-ui`：前端镜像

CI 在 main 分支推送时会把上述镜像构建并推送到 GitHub Container Registry（`ghcr.io/skaduv/`），标签为 `main`；推送 `v*` tag 时生成版本号与 `latest` 标签。

也可以单独构建：`make build-images-app`、`make build-images-docreader`、`make build-images-frontend`。

Lite 模式（单二进制，SQLite + 内存队列，适合本机小规模使用）：

```bash
make build-lite    # 构建
make run-lite      # 运行
```

## 配置说明

- 主配置通过环境变量注入，完整清单见 [.env.example](.env.example)。常用项：
  - `PRIMIND_*`：服务端口、数据库、向量库、解析引擎等运行参数。
  - `SYSTEM_AES_KEY`：数据库敏感字段加密主密钥（32 字节），生产必须显式设置并妥善保管。
  - `JWT_SECRET`：登录令牌签名密钥，留空时启动随机生成。
- 模型厂商与默认地址示例见 `config/builtin_models.yaml.example`、`config/models.json.example`。
- 前端文档入口通过构建时环境变量 `VITE_DOCS_BASE_URL` 配置；未配置时不显示文档入口。

## 版本

当前版本 0.1.0（内部使用）。
