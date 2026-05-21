## Goal
交叉对比本地 CLIProxyAPI / NewAPI 源码仓库与 tx 运行侧，明确源码差异、部署/运行差异、哪些内容可提交、哪些只能保留为 tx 数据/部署资产。

## Active Task List
- [x] 采集本地 NewAPI/CLIProxyAPI 源码仓库状态与关键文件 hash
- [x] 采集 tx NewAPI/CLIProxyAPI 运行目录、Git 状态、容器镜像与关键文件 hash
- [x] 拉取 tx 非敏感关键文件/补丁包，和本地逐文件交叉比对
- [x] 汇总结论：哪些应作为源码提交、哪些仅为 tx 数据/部署资产、迁移清理建议

## Progress
### Done
- [x] CLIProxyAPI 本机源码仓库：`/vol3/root_backup/CLIProxyAPI/CLIProxyAPI`，分支 `fix/account-pool-online-reload`，HEAD `ab4f546caac362d0619b9d0d135e794dfaca9d03`，工作区干净，已推送到 `origin/fix/account-pool-online-reload`。
- [x] CLIProxyAPI 本地关键文件 hash 已采集：`account_pool_auto_append.go`=`3d0308e...`，`account_pool_auto_append_test.go`=`cea930d...`，`docker-compose.tx.yml`=`d84bd15...`，`docker/Dockerfile.runtime`=`f54fd014...`，`docker/nginx/cliproxy.conf`=`56bb3e37...`。
- [x] CLIProxyAPI tx 运行目录 `/root/CLIProxyAPI` 已确认不是 Git 仓库；运行容器 `cli-proxy-api` 使用镜像 `cliproxy-api:local`，端口 `8317`；主机二进制 hash `9389d47...`，容器内二进制 hash `46c491c...`，二者不同。
- [x] CLIProxyAPI 交叉文件比对完成：tx 的 `docker-compose.yml`、`docker/Dockerfile.runtime`、`docker/nginx/default.conf` 与本地模板均有差异；本地模板已参数化镜像/路径/端口，且 nginx 模板只保留 cliproxy server，不包含 tx 上混合的 `pixelle`、`ai/api.muling.store`、`fanqie` 反代段。
- [x] NewAPI 本机 fork：`/vol3/root_backup/github-work/new-api-zhumengstar`，remote `git@github.com:zhumengstar/new-api.git`，分支 `main`，HEAD/origin `0cf288b2962a789eceded23b21bcb95503677ba8`，存在未提交源码改动与构建日志/镜像标记等未跟踪文件。
- [x] NewAPI tx `/root/new-api` 为 Git 工作区但 remote 指向 `https://github.com/QuantumNous/new-api.git`，HEAD `dac55f0fdeb16bbbc2bdc472bda14e60431f3845`，且包含大量部署、审计、回滚、镜像包与本地补丁文件；运行容器 `new-api` 使用镜像 `new-api:local-prebuild-20260521081455-0cf288b2`，端口 `18081:3000`。
- [x] NewAPI tx 源码包 `/root/new-api/newapi-channel-health-src.tgz` 已拉取并与本机 fork 逐文件比对；核心渠道健康补丁文件 hash 完全一致：`controller/channel-test.go`、`controller/relay.go`、`service/channel.go`、`service/channel_health_test.go`、`setting/operation_setting/monitor_setting.go`、default/classic 前端设置文件均一致。
- [x] NewAPI 差异边界已确认：`tasks.md` 不一致，tx 版本多了线上本地二进制替换和健康检查记录；`docker-compose.yml` 不一致，tx 版本使用生产镜像/端口，local fork 仍是通用默认 compose。

### In Progress
- [x] 无。

### Pending
- [ ] 如需部署：按“fn 构建 -> 发布到 tx”的方式重新构建 NewAPI/CLIProxyAPI 镜像或二进制，tx 只保留数据/配置/日志/镜像/部署资产。
- [ ] 如需清理 tx：先做完整备份，再删除或迁移 `/root/new-api` 中源码 Git 工作区，保留 `data/`、`logs/`、`images/`、compose、回滚包等部署资产；CLIProxyAPI 同理保留 `config.yaml`、`auths/`、`logs/`、镜像/二进制、compose。

## Key Findings (Current)
- CLIProxyAPI：本地是干净源码仓库且已提交推送；tx 是运行目录，不是源码仓库。tx 二进制/容器内二进制与本地源码 commit 不能简单等价。
- NewAPI：tx 上当前运行镜像 tag 带本机 fork commit `0cf288b2`，且 tx 导出的渠道健康源码补丁与本机 fork 一致；但 tx Git remote 不是我们的 fork，而是 `QuantumNous/new-api.git`，所以 tx 不能作为提交源。
- 源码差异和部署差异必须分开：NewAPI 核心补丁源码本地与 tx 一致；compose、任务记录、审计/回滚/镜像包属于部署/运行差异，不应直接提交为源码。
- 后续统一遵守：tx 不放源码仓库；NewAPI/CLIProxyAPI 都在 fn/本机 canonical fork 构建，再发布产物到 tx。

## Evidence Files
- 本地采集：`/tmp/local_cross_compare_20260521_183858.txt`
- tx 采集：`/tmp/tx_cross_compare_20260521_183925.txt`
- 拉取与逐文件比对：`/tmp/cross_compare_tx_pull_20260521_184006/report.txt`

## Next Step
- NewAPI 核心源码补丁已提交到 `zhumengstar/new-api`；如用户确认部署，再执行 fn 构建、产物发布到 tx、容器重建与健康检查。
