# CLIProxy 项目继续分析报告

生成时间：2026-05-26
分析范围：`/vol3/root_backup/projects/apps/CLIProxyAPI/CLIProxyAPI`

## 1. 项目定位

CLIProxyAPI 是一个 Go 后端 + React 管理中心的多账号 AI CLI 代理服务，主要能力包括：

- 对外提供 OpenAI / Claude / Gemini / Codex / Antigravity 等兼容 API。
- 使用 OAuth / token 文件管理多个账号。
- 支持多账号轮询、禁用、优先级、代理、headers、模型别名等配置。
- 内置管理 API 和管理前端。
- 目前已新增/存在 Postgres store，用于配置与 auth JSON 的中心化存储。

README 明确说明管理前端源码在 `management-center/`，部署时保留挂载的 `config.yaml`、`auths/`、`logs/`，只替换 binary 和 web assets。

## 2. 代码规模与目录结构

排除 `.git/node_modules/dist/build/cache` 后：

- 文件数：909
- Go 文件：507 个，约 150807 行
- TS/TSX：190 个，约 43709 行
- SCSS/CSS：58 个，约 14117 行
- Markdown：16 个

主要目录：

- `cmd/server/`：主服务入口，解析 flags/env，启动 API server，选择本地/PG/Git/Object store。
- `internal/`：后端核心实现。
  - `internal/api/`：Gin server、路由、middleware、management handler。
  - `internal/api/handlers/management/`：管理 API、账号池、auth 文件、用量统计等最大热点。
  - `internal/store/`：Postgres/Git/Object token store。
  - `internal/runtime/executor/`：各 provider 执行器，如 Claude、Codex、Antigravity、Gemini。
  - `internal/translator/`：协议/模型请求转换。
  - `internal/config/`：配置结构和解析。
  - `internal/logging/`：请求日志。
  - `internal/redisqueue/`：Redis-compatible usage queue。
- `sdk/`：可嵌入 SDK、auth manager、OpenAI/Claude/Gemini handlers。
- `management-center/`：React 管理前端。
- `.deploy/`：当前构建/部署产物，包括 binary 和 static assets。

最大热点文件：

- `internal/api/handlers/management/auth_files.go`：6846 行，账号文件/账号池核心。
- `management-center/src/pages/AccountPoolPage.tsx`：3627 行，账号池前端页。
- `sdk/cliproxy/auth/conductor.go`：4679 行，auth 调度/选择核心。
- `internal/api/handlers/management/account_pool_usage.go`：1690 行，用量 SQLite 核心。
- `internal/api/server.go`：1477 行，server/路由/middleware。

## 3. 存储现状

### 3.1 当前运行时持久数据

线上 tx 当前主路径：

- `/root/CLIProxyAPI/config.yaml`
- `/root/CLIProxyAPI/auths/`
- `/root/CLIProxyAPI/auths/account-pool.sqlite`
- `/root/CLIProxyAPI/auths/account-pool-usage.sqlite`
- `/root/CLIProxyAPI/auths/account-pool.zip`
- `/root/CLIProxyAPI/logs/`

当前线上容器还没有切 PG store env，因此运行时仍然主要使用本地文件/SQLite。

### 3.2 已迁移到 Supabase PG 的数据

schema：`cliproxy`

运行时 PG store 表：

- `cliproxy.config_store`：1 行
- `cliproxy.auth_store`：32 行

归档/分析表：

- `cliproxy.account_pool_entries_archive`：19 行
- `cliproxy.account_pool_folders_archive`：168 行
- `cliproxy.account_pool_usage_records_archive`：6684 行
- `cliproxy.account_pool_usage_summaries_archive`：19 行
- `cliproxy.binary_artifacts`：1 行，保存 `account-pool.zip`

重要：当前 CLIProxy 代码内置 PG store 只直接消费 `config_store/auth_store`，不会自动使用 `_archive` 表替代账号池 SQLite。

## 4. Postgres store 行为

`cmd/server/main.go` 支持这些 env：

- `PGSTORE_DSN`
- `PGSTORE_SCHEMA`
- `PGSTORE_LOCAL_PATH`

当 `PGSTORE_DSN` 非空：

- 启用 PostgresStore。
- 禁用 Git store。
- 以 `PGSTORE_LOCAL_PATH` 或 writable/cwd 作为 spool 根目录。

`internal/store/postgresstore.go` 的行为：

- `EnsureSchema` 创建：
  - `config_store(id, content, created_at, updated_at)`
  - `auth_store(id, content JSONB, created_at, updated_at)`
- `Bootstrap`：
  - 先确保 schema/table。
  - 从 PG 同步 config 到本地 spool config。
  - 从 PG 同步 auth JSON 到本地 spool auths。
- `Save`：
  - 保存 auth JSON 到本地文件。
  - 再 upsert 到 PG `auth_store`。

因此切 PG 后的数据路径会变成：PG 是配置/auth 的上游，服务本地仍有 spool 镜像供旧文件工作流使用。

## 5. 账号池与用量模块

### 5.1 账号池主库

账号池逻辑在 `auth_files.go` 中，当前使用 SQLite：

- 文件：`auths/account-pool.sqlite`
- 表：
  - `account_pool_entries`
  - `account_pool_folders`
- 同时维护 zip mirror：`auths/account-pool.zip`

关键列：

- `account_pool_entries`
  - `name`
  - `content_hash`
  - `type`
  - `provider`
  - `email`
  - `folder`
  - `size`
  - `data`
  - `created_at`
  - `updated_at`
  - `check_result`
  - `check_content_hash`
  - `check_updated_at`
  - `account_started_at`
  - `account_stopped_at`
- `account_pool_folders`
  - `folder`
  - `source_model`
  - `source_info`
  - `created_at`
  - `updated_at`

### 5.2 用量库

用量逻辑在 `account_pool_usage.go` 中，当前使用 SQLite：

- 文件：`auths/account-pool-usage.sqlite`
- 表：
  - `account_pool_usage_records`
  - `account_pool_usage_summaries`

用量字段包含请求时间、request_id、用户、provider、model、auth_id、成功状态、latency、token、cache token、request_params 等。

### 5.3 定时任务

源码常量：

- 账号池高 quota 自动追加：`5 * time.Minute`
- 账号池自动检测：`20 * time.Minute`
- 管理登录尝试 cleanup：`1 * time.Hour`

自动检测会读取 `account_pool_entries`，进行 quota/API 检测，并更新 `check_result` 等字段。自动追加会根据检测结果把高 quota 账号追加到当前运行 auth。

## 6. 管理前端耦合

`management-center/src/pages/AccountPoolPage.tsx` 是账号池 UI 主页面，逻辑较重：

- 导入 zip/tar/gz/json。
- 前端过滤/分页/排序/文件夹视图。
- quota 检测并回写结果。
- 批量上传/删除/下载。
- 账号成本、来源渠道、开始/停止时间等编辑。

`management-center/src/services/api/authFiles.ts` 主要接口：

- `GET /auth-files`
- `PATCH /auth-files/status`
- `PATCH /auth-files/fields`
- `POST /auth-files`
- `DELETE /auth-files`
- `GET /auth-files/download`
- `GET /account-pool/download`
- `POST /account-pool/download`
- `GET /account-pool/download-entry`
- `DELETE /account-pool/delete`
- `GET /account-pool/usage-records`
- 以及 account-pool check/patch/job 类接口

前端本身不关心底层是 SQLite 还是 PG，只要后端 management API 行为兼容即可。

## 7. PG 化缺口

当前已经完成的是：

- config/auth JSON 可被 PG store 接管。
- 当前账号池 SQLite 和 usage SQLite 已同步到 PG archive 表，便于备份/分析。

尚未完成的是：

1. 账号池运行时仍然直接读写 SQLite。
   - `openAccountPoolSQLiteLocked`
   - `readAccountPoolSQLiteLocked`
   - `writeAccountPoolSQLiteLocked`
   - `writeAccountPoolZipMirrorLocked`
   - 自动检测和自动追加都依赖 SQLite 查询。

2. 用量运行时仍然直接读写 SQLite。
   - `accountPoolUsage.Configure(path)` 只接收 SQLite path。
   - 记录、汇总、查询都在 SQLite。

3. zip mirror 仍然是文件语义。
   - 当前 PG 中 `binary_artifacts` 只是归档。
   - 程序不会读取它作为 zip mirror。

4. 日志仍是本地文件。
   - request log 和普通 log 不适合不脱敏直接进 PG。
   - 如需分析，建议做单独脱敏 pipeline。

## 8. 风险点

### P0/P1 风险

- 直接切 `PGSTORE_DSN` 前，必须确认 PG 中 `config_store/auth_store` 是最新状态，否则 PG 会覆盖本地 spool 视角。
- 切 PG 只会管 config/auth，不会让账号池 SQLite 自动消失；账号池仍依赖本地 `auths` 目录中的 SQLite。
- Supabase 直连在某些环境会解析 IPv6；tx Docker 容器里曾出现 IPv6 network unreachable。上线容器切 PG 前必须确认容器内可连 DB，必要时用 Supabase pooler/IPv4 连接串。
- `auth_files.go` 巨大且聚合过多职责，账号池改 PG 时容易误伤上传/下载/编辑/检测/自动追加链路。

### P2 风险

- `AccountPoolPage.tsx` 前端逻辑过重，未来字段变化容易出现前后端回显不一致。
- usage records 中 `request_params` 可能含敏感请求上下文，不建议开放给非管理者或不脱敏复制。
- 历史备份目录中有大量旧 JSON/zip/sqlite，不应混入当前运行数据，除非明确做历史归档。

## 9. 推荐改造路线

### 阶段 A：安全切 config/auth 到 PG

目标：只让 CLIProxy 使用 PG 管理配置和 auth JSON，账号池仍留 SQLite。

步骤：

1. 切换前再次从线上同步最新 `config.yaml` 和 `auths/*.json` 到 PG。
2. 在 tx 容器内验证 DSN 可连，必要时换 Supabase pooler。
3. 给容器增加：
   - `PGSTORE_DSN`
   - `PGSTORE_SCHEMA=cliproxy`
   - `PGSTORE_LOCAL_PATH=/CLIProxyAPI` 或独立 spool 路径
4. 重启。
5. 验证：
   - 日志出现 `postgres-backed token store enabled`。
   - `/admin` 可用。
   - auth 数量一致。
   - 真实 `/v1/chat/completions` smoke test 成功。
   - 修改 auth 字段后 PG `auth_store` 更新时间变化。

### 阶段 B：抽象账号池存储接口

目标：先重构，不改变行为。

新增接口，例如：

- `AccountPoolStore`
  - `ListEntries`
  - `GetEntry`
  - `UpsertEntries`
  - `DeleteEntries`
  - `ListFolders`
  - `UpsertFolder`
  - `PatchCheckResults`
  - `ExportArchive`
- `UsageStore`
  - `Record`
  - `ListRecords`
  - `Summaries`
  - `Clear`

先实现 SQLite adapter，跑通现有测试。

### 阶段 C：实现 Postgres AccountPoolStore

目标：让 `_archive` 表升级为正式运行表，或者新建无 `_archive` 表。

建议正式表名：

- `cliproxy.account_pool_entries`
- `cliproxy.account_pool_folders`
- `cliproxy.account_pool_usage_records`
- `cliproxy.account_pool_usage_summaries`
- `cliproxy.binary_artifacts` 或取消 zip mirror 依赖

注意：

- 需要保留 SQLite 兼容层，方便回滚。
- data 字段建议 PG 用 `JSONB` 而非 TEXT，但要处理原始 JSON 保真和 hash 兼容。
- content_hash 计算必须与现有 zip/JSON 逻辑一致。
- 所有 PATCH 字段要保持前端回显一致。

### 阶段 D：日志/分析单独 pipeline

目标：不要把原始 logs 全量进业务 PG。

建议：

- `request_log_events_sanitized`
- `account_pool_detection_events`
- 只保存脱敏字段、hash、状态码、耗时、model、provider、token 统计。

## 10. 下一步建议

如果当前目标是“尽快稳定切 PG”：

1. 先只切 `config_store/auth_store`。
2. 账号池 SQLite 保持本地运行，PG archive 作为备份。
3. 做容器内 Supabase 连接验证；如 IPv6 失败，换 pooler DSN。
4. 切完跑 smoke tests。

如果目标是“彻底数据库化”：

1. 先做 `AccountPoolStore`/`UsageStore` 抽象。
2. 再实现 PG adapter。
3. 最后迁移前端无感切换。

优先级建议：

- P0：确认容器内 PG 连接方案。
- P1：切 config/auth 到 PG 并回归。
- P1：为账号池 SQLite 增加定期同步到 PG archive 的 cron/脚本，避免 PG archive 过期。
- P2：重构账号池 store 接口。
- P2：实现 PG 账号池运行时存储。
- P3：脱敏日志分析入库。
