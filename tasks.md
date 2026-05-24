## Goal
继续修复/验证 generate.muling.store / sf 上 cliproxy 账号同步报错问题，并在必要时把账号同步到当前运行的 CLIProxyAPI/相关服务；同时确认/开启 cliproxy 远程访问，并按用户要求把两个 cliproxy 服务的管理秘钥/API key 调整为 `admin`。

## Active Task List
- [x] 接续历史排查结论：之前本地代码侧保存链路测试已通过
- [x] 核对 sf/tx 当前运行容器、端口与 CLIProxyAPI 服务状态
- [x] 定位 sf 同步请求返回 401/500 的具体运行时配置
- [x] 对齐 management secret / 运行时配置，避免泄露密钥
- [x] 执行账号同步并验证 /v0/management/auth-files 不再 401
- [x] 验证 generate.muling.store 同步入口恢复，并记录证据
- [x] 开启/确认 cliproxy 远程访问
- [x] 将 sf/tx 两个 CLIProxyAPI 的 remote-management secret 改为 `admin`
- [x] 将 sf/tx 两个 CLIProxyAPI 的 API keys 增加 `admin`，使 `/v1/models` 可用 `Bearer admin`
- [x] 同步 `chatgpt2api` 与 `images-generate` 的 CPA pool secret 为 `admin`
- [x] 定位 generate.muling.store / images-generate 服务的 cliproxy 同步入口和配置位置
- [x] 执行 generate 服务侧 cliproxy 账号配置同步
- [x] 验证 generate 服务能读取同步后的 cliproxy账号配置并无 401/500
- [x] 查明 generate 同步跳过 2 个账号原因：源端 3 个 token 中本地已有 2 个，`add_accounts()` 按 token 去重后返回 `added=1, skipped=2`
- [x] 为 generate CPA/cliproxy 远程账号列表补后端删除代理接口 `/api/cpa/pools/{pool_id}/files/delete`
- [x] 热补并重启 `images-generate` 后端，验证删除不存在远程文件返回 `200` 且 `deleted=0 failed=1 HTTP 404`，未误删真实账号
- [ ] 前端“删除选中远程账号”按钮源码已补，正在本机低风险构建静态产物并准备同步回 sf

## Progress
### Done
- [x] 已确认本地仓库 `/vol3/root_backup/projects/apps/CLIProxyAPI/CLIProxyAPI` 分支 `fix/account-pool-online-reload` 的后端账号池保存链路测试通过。
- [x] 已加载 CLIProxyAPI account-pool save / zero-downtime 参考流程。
- [x] sf (`165.154.254.130`) 容器状态：`chatgpt2api` 映射 `3000->80`，`cli-proxy-api` 映射 `8317->8317`，服务在线。
- [x] tx (`43.173.104.221`) 容器状态：`cli-proxy-api` 在线，`8317` 监听正常。
- [x] 修复 sf `chatgpt2api` 的 CPA pool management secret 配置；此前 `bda467a5c680` 访问远端 auth-files 返回 401 并导致 app 侧 500。
- [x] 将 sf 本机 CLIProxyAPI 账号池写入运行时 auth files：`account_pool_entries=6`，`/v0/management/auth-files` 返回 6 个文件。
- [x] 将 tx CLIProxyAPI 账号池写入运行时 auth files：`account_pool_entries=412`，`/v0/management/auth-files` 返回 412 个文件。
- [x] 通过 sf `chatgpt2api` 授权接口验证：pool `778af22671e6` 返回 412 个文件；pool `bda467a5c680` 返回 6 个文件。
- [x] 确认 sf/tx CLIProxyAPI `remote-management.allow-remote=true`，management secret 已配置。
- [x] 确认 sf/tx CLIProxyAPI 端口 `8317` 均绑定 `0.0.0.0`；tx 公网 IP 直连可达，sf 通过域名 `cliproxy.1997121.xyz` 可达。
- [x] 已备份并修改 sf/tx `/root/CLIProxyAPI/config.yaml`：`remote-management.secret-key` 改为 `admin`。
- [x] 已在 sf/tx `/root/CLIProxyAPI/config.yaml` 的 `api-keys` 中加入 `admin`，让 `/v1/*` 也可用 `Bearer admin`。
- [x] 已同步 sf 的 `/root/chatgpt2api/data/cpa_config.json` 与 `/root/images-generate-for-phone/data/cpa_config.json`，两个 pool 的 `secret_key` 均改为 `admin`，并重启 `chatgpt2api` / `images-generate`。
- [x] sf 修改 `api-keys` 时曾短暂产生 YAML 格式错误，已校正并重启恢复；日志显示服务已成功加载 6 clients。
- [x] generate 后端已有本地账号删除接口 `DELETE /api/accounts`；本次新增的是 CPA 远程账号删除代理。
- [x] 新增并热补 `services/cpa_service.py::delete_remote_files()` 与 `api/accounts.py` 的 `/api/cpa/pools/{pool_id}/files/delete`。
- [x] 后端删除代理真实验证：删除不存在文件返回 `200`，结果 `deleted=0 failed=1 HTTP 404`，并返回远程剩余文件列表，说明不会误删真实账号。

### In Progress
- [ ] 正在生成并同步前端静态产物，让设置页 CPA 导入弹窗显示“删除选中远程账号”。

### Pending
- [ ] 清理 sf 现有重复账号文件后复测同步流程。
- [ ] DockerHub 访问仍受 sf Tailscale DNS 影响；已给 Docker daemon 增加公共 DNS 备份可回滚，但 Docker CLI 解析仍走 Tailscale IPv6，因此前端改用本机构建后同步静态产物。

## Evidence
- sf domain management: `https://cliproxy.1997121.xyz/v0/management/auth-files` + `Bearer admin` => HTTP 200，6 files。
- sf domain API: `https://cliproxy.1997121.xyz/v1/models` + `Bearer admin` => HTTP 200，8 models。
- tx direct management: `http://43.173.104.221:8317/v0/management/auth-files` + `Bearer admin` => HTTP 200，412 files。
- tx direct API: `http://43.173.104.221:8317/v1/models` + `Bearer admin` => HTTP 200，8 models。
- sf `chatgpt2api` pool files after admin sync: `778af22671e6` => 412 files；`bda467a5c680` => 6 files。
- sf `images-generate` pool files after admin sync: `778af22671e6` => 412 files；`bda467a5c680` => 6 files。
- generate remote delete proxy smoke: `POST /api/cpa/pools/778af22671e6/files/delete` => HTTP 200, `deleted=0 failed=1`, first error `HTTP 404` for nonexistent probe。
- sf direct public IP `http://165.154.254.130:8317` 从外部探测超时，疑似云防火墙/安全组未放行；但域名路由已可远程访问。
- recent logs show `GET /v0/management/auth-files` and `GET /v1/models` with public source returned 200。

## Key Findings (Current)
- `/v0/management/*` 使用的是 management secret；`/v1/models` 使用的是普通 API key。现在两类 key 都已配置 `admin`，所以两类接口都可以用 `Authorization: Bearer ***`。
- cliproxy 远程访问已开启：management/API 均可通过外部路由到达服务。
- sf 的直连公网 IP:8317 从外部超时，但域名 `cliproxy.1997121.xyz` 已可用；如果必须 IP 直连，需要在云安全组/防火墙额外放行 8317。
- generate 同步时 `skipped=2` 是本地去重结果，不是远端跳过：3 个远程 token 中已有 2 个在本地账号池中。
- 为避免 CPU/磁盘问题，没有在 sf 上强行完整构建前端镜像；当前改为本机构建静态产物后小文件同步。
- 所有排查输出中涉及 key/token/secret 的具体值除用户指定的 `admin` 外不记录。

## Next Step
- 等本机前端静态构建完成后，同步 `web/out` 到 sf 容器 `/app/web_dist`，验证页面包含“删除选中远程账号”按钮。
- 清理 sf 重复账号文件并复测同步。