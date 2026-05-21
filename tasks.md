## Goal
合并本机 CLIProxyAPI 源码补丁与 tx 运行侧非敏感部署资产，在本机验证后提交并推送到代码仓库。

## Active Task List
- [x] 备份/固定当前本机 CLIProxyAPI 工作区状态，确认待合并范围
- [x] 从 tx 拉取非敏感部署文件并合并到本机代码库（不拉取 auths/config secrets）
- [x] 整理本机未提交补丁与测试，格式化并本机验证
- [x] 审查 diff/敏感信息，提交并推送到 GitHub 代码仓库
- [ ] 汇报 commit、验证结果与剩余注意事项【进行中】

## Progress
### Done
- [x] 已备份本机 dirty 工作区到 `/vol3/root_backup/CLIProxyAPI/merge-backups/local-dirty-20260521_171547.patch`。
- [x] 已确认本机源码仓库：`/vol3/root_backup/CLIProxyAPI/CLIProxyAPI`，分支 `fix/account-pool-online-reload`，原 HEAD `895240e1bbb8f2c377a1bca4153245549db516c7`。
- [x] 已从 tx `/root/CLIProxyAPI` 拉取非敏感部署文件用于合并：`docker-compose.yml`、`docker/Dockerfile.runtime`、`docker/nginx/default.conf`；未拉取 `config.yaml`、`auths/`、日志或任何凭据。
- [x] 已将 tx 运行侧部署资产整理为源码仓库内的 `docker-compose.tx.yml`、`docker/Dockerfile.runtime`、`docker/nginx/cliproxy.conf`。
- [x] 已运行目标 Go 测试：`docker run ... golang:1.26-alpine ... go test ./internal/api/handlers/management -run "TestAppendHighQuotaAccountPoolEntriesSkipsDuplicateContent|TestHighQuotaAccountPoolCandidatesFiltersThresholdAndStaleChecks|TestHighQuotaRemainingFromCheckRequiresUsableStatusAndPercent|TestMarkAuthDeletedFromDiskHidesListButKeepsRuntimeRecord" -count=1 -v`，结果 PASS。
- [x] 已运行完整 management 包测试：`go test ./internal/api/handlers/management -count=1 -v`，结果 PASS。
- [x] 已运行编译验证：`go build -o /tmp/cli-proxy-api-test ./cmd/server`，结果 PASS。
- [x] 已运行 `docker compose -f docker-compose.tx.yml config`，结果 OK。
- [x] 已尝试运行全仓 `go test ./... -count=1`；当前失败项为既有无关测试：`internal/registry TestCodexFreeModelsExcludeGPT55`，`internal/runtime/executor TestEnsureAccessToken_WarmTokenLoadsCreditsHint` 与 `TestUpdateAntigravityCreditsBalance_LoadCodeAssistUserAgent`。本次修改包 `internal/api/handlers/management` 通过。
- [x] 已完成 secret scan：仅命中测试中的 `MANAGEMENT_PASSWORD` 空值和任务说明文字，未发现实际密钥。
- [x] 已提交并推送：`15ecbd9caadbef3dc7c0dfd649a65de22c34c475` (`fix: dedupe auto-appended account pool auths`) 到 `origin/fix/account-pool-online-reload`。

### In Progress
- [ ] 汇报 commit、验证结果与剩余注意事项。

### Pending
- [ ] 如需部署 tx，下一步应从该 commit 构建新二进制/镜像并按保留配置/数据方式发布。

## Key Findings (Current)
- tx 运行目录不是 Git 仓库，应只合并部署模板/运行结构，不覆盖生产数据。
- 当前仓库新增部署模板使用环境变量参数化路径/端口，避免提交 tx 的真实 `config.yaml` 与 `auths/`。
- 本机 Git 状态已干净，`HEAD` 与 `origin/fix/account-pool-online-reload` 均为 `15ecbd9c`。

## Next Step
- 向用户汇报结果；如用户确认部署，再执行构建、镜像上传/加载、容器重建与健康检查。
