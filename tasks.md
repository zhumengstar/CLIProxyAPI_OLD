## Goal
合并本机 CLIProxyAPI 源码补丁与 tx 运行侧非敏感部署资产，在本机验证后提交并推送到代码仓库。

## Active Task List
- [x] 备份/固定当前本机 CLIProxyAPI 工作区状态，确认待合并范围
- [x] 从 tx 拉取非敏感部署文件并合并到本机代码库（不拉取 auths/config secrets）
- [x] 整理本机未提交补丁与测试，格式化并本机验证
- [ ] 审查 diff/敏感信息，提交并推送到 GitHub 代码仓库【进行中】
- [ ] 汇报 commit、验证结果与剩余注意事项

## Progress
### Done
- [x] 已备份本机 dirty 工作区到 `/vol3/root_backup/CLIProxyAPI/merge-backups/local-dirty-20260521_171547.patch`。
- [x] 已确认本机源码仓库：`/vol3/root_backup/CLIProxyAPI/CLIProxyAPI`，分支 `fix/account-pool-online-reload`，HEAD `895240e1bbb8f2c377a1bca4153245549db516c7`。
- [x] 已从 tx `/root/CLIProxyAPI` 拉取非敏感部署文件用于合并：`docker-compose.yml`、`docker/Dockerfile.runtime`、`docker/nginx/default.conf`；未拉取 `config.yaml`、`auths/`、日志或任何凭据。
- [x] 已将 tx 运行侧部署资产整理为源码仓库内的 `docker-compose.tx.yml`、`docker/Dockerfile.runtime`、`docker/nginx/cliproxy.conf`。
- [x] 已运行目标 Go 测试：`docker run ... golang:1.26-alpine ... go test ./internal/api/handlers/management -run "TestAppendHighQuotaAccountPoolEntriesSkipsDuplicateContent|TestHighQuotaAccountPoolCandidatesFiltersThresholdAndStaleChecks|TestHighQuotaRemainingFromCheckRequiresUsableStatusAndPercent|TestMarkAuthDeletedFromDiskHidesListButKeepsRuntimeRecord" -count=1 -v`，结果 PASS。
- [x] 已运行完整 management 包测试：`go test ./internal/api/handlers/management -count=1 -v`，结果 PASS。
- [x] 已运行编译验证：`go build -o /tmp/cli-proxy-api-test ./cmd/server`，结果 PASS。
- [x] 已运行 `docker compose -f docker-compose.tx.yml config`，结果 OK。
- [x] 已尝试运行全仓 `go test ./... -count=1`；当前失败项为既有无关测试：`internal/registry TestCodexFreeModelsExcludeGPT55`，`internal/runtime/executor TestEnsureAccessToken_WarmTokenLoadsCreditsHint` 与 `TestUpdateAntigravityCreditsBalance_LoadCodeAssistUserAgent`。本次修改包 `internal/api/handlers/management` 通过。

### In Progress
- [ ] 审查最终 diff 与 secret scan，然后提交并推送。

### Pending
- [ ] 汇报 commit、验证结果与剩余注意事项。

## Key Findings (Current)
- tx 运行目录不是 Git 仓库，应只合并部署模板/运行结构，不覆盖生产数据。
- 当前仓库新增部署模板使用环境变量参数化路径/端口，避免提交 tx 的真实 `config.yaml` 与 `auths/`。
- secret scan 仅命中测试中的 `MANAGEMENT_PASSWORD` 环境变量空值和任务说明文字，未发现实际密钥。

## Next Step
- 最终 `git diff` 审查后 commit + push 到 origin。
