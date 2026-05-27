# NewAPI Supabase 数据放置检查

- [x] 确认 NewAPI 当前部署配置与数据库连接目标
  - new-api 容器当前 SQL_DSN 指向 Supabase pooler。
- [ ] 盘点本地/旧 Postgres 与 Supabase 数据表及行数差异
- [ ] 检查是否还有卷、SQLite、文件上传等数据未迁移到 Supabase
- [ ] 汇总结论和风险项

# CLIProxyAPI 使用记录 Supabase 读取修复

- [x] 确认线上使用记录数据未丢失：Supabase/PostgreSQL `cliproxy.account_pool_usage_records` 为 13616 条，`account_pool_usage_summaries` 为 283 条，summary requests 为 13616，总 token 非 0。
- [x] 确认截图 0 条的后端根因：使用记录页面 GET 路径此前仍主要读取本地 SQLite/内存 recorder，而不是 Supabase 优先；因此页面可能显示空缓存/空内存结果。
- [x] 修复后端：`GET /account-pool/usage-records` 改为 Supabase/PostgreSQL 优先读取，SQLite recorder 作为 fallback，并在响应中返回 `source=postgres`。
- [x] 兼容历史 SQLite fallback：本地加载 SQL 对可空字段使用 COALESCE，避免历史 NULL 导致扫描失败后内存保持空。
- [x] 运行 `/usr/local/go1.26/bin/go test ./internal/api/handlers/management` 通过，并完成 Linux amd64 静态二进制构建。
- [x] 热替换线上真实二进制 `/CLIProxyAPI/CLIProxyAPI` 并重启容器，基础接口 `/` 返回 200，`/v1/models` 未带 key 返回 401，容器运行正常。
- [x] 验证线上日志未出现 usage PG fallback/panic；PG 侧计数确认完整。

# CLIProxyAPI 使用记录从原始 SQLite 恢复

- [x] 按用户要求“从原先的地方”恢复使用记录数据，原始来源确认为 `/root/CLIProxyAPI/auths/account-pool-usage.sqlite`。
- [x] 恢复前核对本地运行库仍完整：`account_pool_usage_records=13616`，时间范围 `2026-05-16T22:41:24+08:00` 到 `2026-05-26T02:03:40+08:00`，`account_pool_usage_summaries=283`。
- [x] 恢复前核对 Supabase 当前状态：`cliproxy.account_pool_usage_records=13616`，`account_pool_usage_summaries=283`，summary requests 为 13616。
- [x] 执行事务性覆盖恢复：从原始 SQLite 导出 records/summaries，导入 Supabase 前自动创建恢复前备份表，备份后缀为 `20260526_115035`。
- [x] 恢复后验证 Supabase：records=13616，summaries=283，summary requests=13616，总 token=1059743835，first_id=2654，last_id=22270。
- [x] 恢复后验证本地 SQLite：records=13616，summaries=283，总 token=1059743835，与 Supabase 聚合一致。
- [x] 服务容器保持运行正常，未发现 usage PG fallback/panic。

# CLIProxyAPI / cliproxy Supabase-only 与去重存储修复

- [x] 修复截图异常 `额度请求 400: auth not found`：Supabase-only 后本地 account-pool 文件被移走，但账号池检测/API 工具的按账号名读取仍可能从本地 archive/SQLite 找 auth；已补 `readAccountPoolEntriesFromPostgres` / `readAccountPoolEntryFromPostgres`，账号池检测与 API 工具现在从 Supabase runtime 表读取 auth 数据。
- [x] 修复“账号池导入提示成功但页面不显示”的根因：`upsertAccountPoolArchiveFiles()` 原先在本地 `account-pool.sqlite` 存在时优先写 SQLite 并直接返回，而页面列表在 PG enabled 时读 Supabase/Postgres，导致写入与显示读源分裂；现已改为 `PGSTORE_DSN` 存在时导入直接 upsert `account_pool_entries/account_pool_folders`，不再落本地 SQLite/zip。
- [x] 收口运行读路径：PG enabled 时 `readAccountPoolArchiveEntry` / `readAccountPoolDatabaseLocked` 不再“PG 空则回落本地”，自动检测与高额度自动追加也改为 PG 优先读取，避免线上又读到旧 SQLite/zip。
- [x] 取消账号池 zip/archive 重复存储：`account_pool_entries.data` 已保存账号 JSON，`account_pool_archive_blobs(account-pool.zip)` 是同一份数据的重复副本；已改为写入时只保存 runtime entries/folders，下载 zip 时从 runtime 动态打包。
- [x] 清理 Supabase 旧重复 archive blob：`archive_blobs_before=1`、`archive_blobs_after=0`，核心业务表计数保持 `entries=20`、`folders=169`、`usage_records=13616`、`usage_summaries=283`。
- [x] 增加“每刀成本”实时展示：账号卡片在“成本”旁显示 `账号成本 ÷ 账号已消耗刀数`，目录卡片在“总刀数”旁显示 `目录账号成本合计 ÷ 目录总刀数`；显示随 usage/Supabase 数据实时变化。
- [x] 合并/删除重复 Supabase 业务表：删除未被代码引用的 `account_pool_entries_archive`、`account_pool_folders_archive`、`account_pool_usage_records_archive`、`account_pool_usage_summaries_archive`、`account_pool_usage_records_backup_20260526_115035`、`account_pool_usage_summaries_backup_20260526_115035`，并移除 `account_pool_archive_blobs` 的创建/写入逻辑后删除空表；保留主表 `account_pool_entries`、`account_pool_folders`、`account_pool_usage_records`、`account_pool_usage_summaries`、`auth_store`、`config_store`。
- [x] 前端使用 Node 22 Docker 构建通过，生成 `AccountPoolPage-CBj6DZmY.js`，静态资源已部署到容器 `/CLIProxyAPI/static`，并补 `management.html` 兼容入口。
- [x] 后端 `/usr/local/go1.26/bin/go test ./internal/api/handlers/management` 通过，静态二进制已热替换上线。
- [x] 修正存活计时逻辑：当前实现为状态机累计 `account_lifetime_seconds`，活跃区间由 `account_lifetime_active_since` 表示，页面显示 `累计秒数 + 当前活跃区间(now-active_since)`；401/认证失效停止计时，success/ok/passed 与额度耗尽继续计时。已补 `usage limit has been reached` / `rate_limit` 识别，并将检测结果写入改为 Supabase/PG 优先，SQLite 仅 fallback。
- [x] 修复历史误判：将已被误停的 `usage limit has been reached` 账号重新置为 active；线上校验 `total=20`、`active_since=5`、`stopped=15`、`future_active=0`、`both_active_and_stopped=0`。
- [x] 排查 `auth_unavailable / invalidated oauth token`：直接来源是 PG `auth_store` 的 `0001_QSSY...` 账号被 session-affinity 长 TTL 绑定到 `admin` 会话，批量刷新出现 `refresh_token_reused`；临时隔离会导致剩余 30 个 codex 账号无法服务 `gpt-5.5`，因此已恢复该账号，随后 `gpt-5.5` 最小真实请求返回 HTTP 200 / `pong`。
- [x] 使用记录新写入在 Supabase 可用时直接写 `account_pool_usage_records` / `account_pool_usage_summaries`，成功后不再落本地 SQLite；无 PG 的离线/测试环境仍保留 SQLite fallback。
- [x] 运行 `/usr/local/go1.26/bin/go test ./internal/api/handlers/management` 通过。
- [x] 静态构建 `/tmp/cliproxy-server-supabase-only-20260526121921`，确认 `not a dynamic executable`。
- [x] 线上热替换真实二进制 `/CLIProxyAPI/CLIProxyAPI` 并重启容器。
- [x] 将线上 `/root/CLIProxyAPI/auths/account-pool*` 本地业务文件统一移到备份目录，不保留在运行目录。
  - 主备份目录：`/root/CLIProxyAPI/local-business-backup-20260526_122015`
  - 残留备份目录：`/root/CLIProxyAPI/local-business-backup-20260526_122115-residual-auths`
- [x] 重启后二次验证运行目录未重新生成本地账号池/usage 业务文件：`LOCAL_ACCOUNT_POOL_FILE_COUNT=0`。
- [x] Supabase 验证：`account_pool_entries=20`、`account_pool_folders=169`、`account_pool_usage_records=13616`、`account_pool_usage_summaries=283`、`account_pool_archive_blobs=1`、`config_store=1`、`auth_store=30`。
- [x] 服务健康验证：容器 `Up`，`/` 返回 200，未带 key 的 `/v1/models` 返回 401。
- [x] 收口页面缓存：账号池列表、检查结果、检查 pending、检测并发偏好、导入 job 恢复不再写入或读取页面 localStorage；遗留 `cli-proxy-account-pool*` 键只在加载时清理，不再作为业务事实来源。
- [x] 收口后端 PG-only 失败语义：PG enabled 时账号池列表、单账号读取、账号池数据库读取、检查结果写回、账号编辑、usage 读取不再失败后回落本地 SQLite/zip，而是直接返回错误，避免读写源分裂。
- [x] 本轮本地验证：`/usr/local/go1.26/bin/go test ./internal/api/handlers/management ./internal/api` 通过；Node 22 Docker `npm run build` 通过；静态二进制 `/tmp/cliproxy-server-supabase-only-nocache-static` 已生成并确认 statically linked，sha256 `0c03d39dd7413a2ba1f3f5865ba17684268e93bb0790e67fbdee7928440b8252`。
- [x] 部署本轮 Supabase-only/no-page-cache 改动到线上真实二进制 `/CLIProxyAPI/CLIProxyAPI`，同步最新前端 dist，并验证 `/management.html`、`/admin`、运行目录本地业务文件计数为 0、容器 hash 与本地构建一致。
  - 线上热替换前已备份容器内 `/CLIProxyAPI/CLIProxyAPI`，同步 `/root/CLIProxyAPI/static`，重启后容器 `cli-proxy-api` 正常运行。
  - 验证结果：`/healthz=200`、`/management.html=200`、`/admin=200`、`/admin/=200`，本地业务文件计数 `0`，启动日志包含 `postgres-backed token store enabled` / `API server started successfully on: :8317` / `core auth auto-refresh started`。
  - 管理 API 账号池列表/usage 需要明文管理 key；线上只保存 bcrypt hash 且容器无 `MANAGEMENT_PASSWORD`，因此本轮未强行绕过鉴权验证响应体 `source=postgres`。代码与启动日志已确认 PG enabled，且运行目录无本地账号池/usage 文件。

注意：`/CLIProxyAPI/config.yaml` 仍作为服务启动配置文件存在；账号池和使用记录等业务数据已切到 Supabase 主读写。PG enabled 时不再回落本地 SQLite/zip；本地 SQLite/zip 仅用于无 `PGSTORE_DSN` 的测试/离线环境。

# CLIProxyAPI 账号池错误码展示修复

- [x] 已根据截图确认问题位置：账号池页 `/management.html#/account-pool` 中，卡片的真实模型请求失败 `401 {"detail":"Unauthorized"}` 被展示/统计成主错误码 401。
- [x] 已使用 Claude Code 辅助修复，区分账号检测/额度请求主状态码与真实模型请求 HTTP 状态码，避免真实模型请求 401 污染顶部错误码统计、筛选与卡片主状态。
- [x] 已补充/确认 `accountPoolStatus.test.ts` 覆盖：显式 `statusCode` 优先、`realRequestError` 不生成主状态码、消息中的“额度请求 400”仍兜底提取、模型请求错误摘要显示“模型请求”。
- [x] 本地验证：Node v22.12.0 下 `npm run type-check`、`npx tsx accountPoolStatus.test.ts`、`npm run build` 均通过；后端 `/usr/local/go1.26/bin/go test ./internal/api/handlers/management ./internal/api` 通过。
- [x] 已构建 Linux amd64 静态二进制 `/tmp/CLIProxyAPI-cliproxy-hotfix-20260527081808`，`ldd` 为 `not a dynamic executable`，sha256 `91b51132621edb7d786fbc27080a742a174053de16d452e174e64975e033239b`。
- [x] 已安全部署到 tx 服务器容器 `cli-proxy-api`：部署前备份目录 `/root/cliproxy-hotfix-backups/20260527_081859`，包含 docker inspect/logs、原二进制与原静态资源；原二进制 sha256 `0c03d39dd7413a2ba1f3f5865ba17684268e93bb0790e67fbdee7928440b8252`。
- [x] 已热替换线上 `/CLIProxyAPI/CLIProxyAPI` 并同步前端 dist 到 `/root/CLIProxyAPI/static` / 容器 `/CLIProxyAPI/static`；补 `management.html` 兼容入口；容器重启后运行正常。
- [x] 线上验证：容器 `Up`，`/healthz=200`，`/=200`，未带 key 的 `/v1/models=401`；`/management.html`、`/admin`、`/admin/`、`/assets/AccountPoolPage-DXbdDWFA.js` 均返回 200。
- [x] 静态资源验证：线上 AccountPoolPage JS sha256 `eb4d6d8f86be4621ba28dbb311e11ac5bc4542db143ec1b929308fe3ba9cd98d`，包含 `模型请求` 与 `realRequestStatusCode`，说明错误码归因修复已进入前端产物；近 5 分钟日志未见 panic/fatal/exec/no such file。

# CLIProxyAPI 账号池检测状态 DB 权威同步修复

- [x] 已确认当前状态链路：检测结果通过 `PATCH /account-pool/check-results` 写入数据库 `account_pool_entries.check_*` 字段；页面进入/刷新通过 `/account-pool/list` / `listAccountPoolEntries()` 读回 `check_*` 字段并 hydrate 到前端展示。
- [x] 已进一步收口前端 store：`hydrateResultsFromFiles()` 现在每次都从服务端返回的 `check_*` 字段重建检测结果；除正在检测中的 `loading` 临时态外，不再把旧 Zustand 内存结果作为事实源保留。服务端无 `check_status`、hash 失配、或列表中不存在的账号都会清掉旧状态。
- [x] 已调整 `pruneResults()`：只保留正在检测中的 loading 标记，避免刷新/筛选/列表变更时把旧成功/失败结果重新带回 UI。
- [x] 检测流程保持“先写 DB、再刷新 DB 读回”：`flushRemoteCheckResults()` 会排空 pending 写库队列；检测结束后只有写库成功才调用 `refreshPool(false)` 从数据库重新读取。
- [x] 已补充 `management-center/accountPoolCheckStore.test.ts` 回归：覆盖服务端无 `check_status` 删除旧状态、本地 `checkedAt` 更新也被 DB 覆盖、hash 失配删除旧状态、真实请求失败归一化、以及刷新只用 DB 字段重建状态。
- [x] 验证通过：Node 22 下 `npx tsx accountPoolCheckStore.test.ts`、`npx tsx accountPoolStatus.test.ts`、`npm run type-check`、`npm run build` 均成功；Go `/usr/local/go1.26/bin/go test ./internal/api/handlers/management ./internal/api` 通过。
- [x] 当前最新前端构建产物为 `management-center/dist/assets/AccountPoolPage-BfIfpVvT.js`，sha256 `b64ce98b5f13615587b60775a56967bb44e5cb8f601014f364b1af439f304cf5`。
- [x] 已按“每次改代码后远程部署”要求部署到 tx/`cli-proxy-api`：备份目录 `/root/cliproxy-hotfix-backups/20260527_102822-frontend-db-authoritative-check`，同步最新 `management-center/dist` 到 `/root/CLIProxyAPI/static`，补齐 `/management.html` 与 `/admin/index.html` fallback，并重启容器。
- [x] 线上验证通过：容器 `cli-proxy-api` 正常运行；`/healthz=200`、`/=200`、`/management.html=200`、`/admin=200`、`/admin/=200`、`/assets/AccountPoolPage-BfIfpVvT.js=200`；线上静态资源 sha256 为 `b64ce98b5f13615587b60775a56967bb44e5cb8f601014f364b1af439f304cf5`；近 3 分钟未见 panic/fatal/exec/no such file/migration failed/bind address。

# CLIProxyAPI 账号池检测全部/单个检测一致性修复

- [x] 定位根因：`检测全部` 默认并发为 50，多个账号完成时会高频触发延迟 `PATCH /account-pool/check-results`；结束阶段如果已有一次 flush 正在运行，最终 `flushRemoteCheckResults()` 可能只等待正在进行的 PATCH，而没有继续提交这期间新入队的最后一批结果，随后刷新从 DB 读到的就可能是部分旧状态。单个检测通常只有一个结果，因此更不容易触发该竞态。
- [x] 已修复 flush 逻辑：一次 flush 会循环读取并提交 pending 队列，直到队列为空才返回；如果提交失败则把本批结果放回 pending 并定时重试。这样 `检测全部` 和单个 `请求大模型` 都保证“先写库成功，再刷新后台 DB 状态”的同一语义。
- [x] 已降低检测全部/批量检测默认并发：默认从 50 改为 2，并增加最大并发 5 的上限，符合“可以慢慢检测，并发不用太高”的要求，减少上游限流/鉴权刷新冲突，也降低写库队列竞态概率。
- [x] 已保留单个检测与全部检测共用 `detectAccounts()` / `checkOne()` / `queueRemoteCheckResult()` / `flushRemoteCheckResults()` 这条路径，避免两套状态口径。
- [x] 已补回归测试：`accountPoolCheckStore.test.ts` 覆盖服务端返回 `check_status=success` 但 `check_real_request_ok=false` 时，刷新后必须归一为 `error`，与单个请求大模型失败状态一致。
- [x] 验证通过：Node 22 下 `npx tsx accountPoolCheckStore.test.ts`、`npx tsx accountPoolStatus.test.ts`、`npm run type-check`、`npm run build` 均成功；Go `/usr/local/go1.26/bin/go test ./internal/api/handlers/management ./internal/api` 通过。
- [x] 当前最新前端构建产物为 `management-center/dist/assets/AccountPoolPage-D7NTTHcd.js`。
- [ ] 尚未部署本轮最新前端产物；线上要生效需要同步最新 `management-center/dist` 静态资源并重启/验证。

# CLIProxyAPI 账号池额度 auth not found 误判修复

- [x] 已使用 Claude Code 启动排查本轮问题；Claude Code 会话长时间无输出后终止，后续基于代码链路完成小范围补丁和验证。
- [x] 复核单个按钮与检测全部入口：二者都调用 `detectAccounts()` / `checkOne()`；差异主要不在按钮入口，而在批量检测时额度接口更容易先遇到 `APICall` 的 `$TOKEN$` 解析失败并返回 `400 auth not found`，旧逻辑会在额度阶段直接返回失败，不再用同一账号继续做真实模型请求兜底。
- [x] 已修复 `checkOne()`：额度接口最终失败后，不再立刻把账号判为不可用；会继续调用同一账号的 `requestCodexModelForAccountPool()` 做真实 `gpt-5.4-mini` 请求。真实请求成功则结果记为 `success`、`realRequestOk=true`、`quotaOk=false`，并保留额度接口错误详情，避免“实际可用”账号被 `auth not found` 误杀。
- [x] 真实请求仍失败时才返回错误，并继续保留主 `statusCode` 与 `realRequestStatusCode` 分离：额度接口错误码仍作为主状态码；模型请求错误只写 `realRequestStatusCode` / `realRequestError`，不破坏前面错误码归因修复。
- [x] 验证通过：Node 22 下 `npx tsx accountPoolCheckStore.test.ts`、`npx tsx accountPoolStatus.test.ts`、`npm run type-check`、`npm run build` 均成功；Go `/usr/local/go1.26/bin/go test ./internal/api/handlers/management ./internal/api` 通过。
- [x] 当前最新前端构建产物为 `management-center/dist/assets/AccountPoolPage-BY9itxIO.js`，sha256 `68e35e8a75bc8ddd0edf319227305861741711e110220ece334deebb3e66218f`。
- [x] 已按用户要求远程部署本轮前端静态资源到 tx/`cli-proxy-api`：部署前备份目录 `/root/cliproxy-hotfix-backups/20260527_100213-frontend-auth-not-found`；同步 `/root/CLIProxyAPI/static` 与容器 `/CLIProxyAPI/static`，并重启容器。
- [x] 线上验证通过：`/healthz=200`、`/=200`、`/management.html=200`、`/admin=200`、`/admin/=200`、`/assets/AccountPoolPage-BY9itxIO.js=200`；主机与容器内 AccountPoolPage JS sha256 均为 `68e35e8a75bc8ddd0edf319227305861741711e110220ece334deebb3e66218f`；近 3 分钟日志未见 panic/fatal/exec/no such file/migration failed/bind address。

# CLIProxyAPI 远程仓库代码推送

- [x] 已检查当前分支：`fix/account-pool-online-reload`，上游为 `origin/fix/account-pool-online-reload`，远程仓库为 `git@github.com:zhumengstar/CLIProxyAPI.git`。
- [x] 已确认远程分支与本地提交基线一致：`git rev-list --left-right --count HEAD...@{u}` 为 `0 0`，可以直接提交本轮本地改动。
- [x] 已执行提交前验证：Node 22 下 `npx tsx accountPoolCheckStore.test.ts`、`npx tsx accountPoolStatus.test.ts`、`npm run type-check`、`npm run build` 均通过；Go `/usr/local/go1.26/bin/go test ./internal/api/handlers/management ./internal/api` 通过。
- [x] 已提交并 push 到远程仓库：commit message 为 `fix(account-pool): persist check state and usage in postgres`，已推送到 `origin/fix/account-pool-online-reload`。
- [x] 已验证远程分支与本地 `HEAD` 一致，本地与上游 divergence 为 `0 0`，工作区干净。