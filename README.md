# CPA Usage Patch

给新版 CLIProxyAPI 恢复“原汁原味”的内置使用统计页面和后端统计逻辑。

这个项目已经整理成可直接放到 GitHub 的独立仓库形态，构建和发布通过 GitHub Actions 完成，目标平台与 CLIProxyAPI 保持一致：

- `darwin/amd64`
- `darwin/arm64`
- `freebsd/amd64`
- `freebsd/arm64`
- `linux/amd64`
- `linux/arm64`
- `windows/amd64`
- `windows/arm64`

## 功能

1. 自动给 CLIProxyAPI 的 `management.html` 注入本地 loader。
2. 在 CPAMC 左侧恢复“使用统计”导航项。
3. 复用旧版 CPAMC 的使用统计页面，而不是重写一个简化版。
4. 自动打开现版 CLIProxyAPI 后端仍保留的 `usage-statistics-enabled` 开关。
5. 持续消费现版 CLIProxyAPI 仍保留的 RESP usage queue。
6. 将统计持久化到本地 `cpa-usage-patch-records.jsonl`。
7. 补回旧版接口：
   - `/v0/management/usage`
   - `/v0/management/usage/export`
   - `/v0/management/usage/import`

## 部署方式

把 `cpa-usage-patch` 放到 `cli-proxy-api` 同级目录即可。这里故意不写平台后缀，因为不同平台后缀不同，但文件名主体一致。

补丁会自动搜索这些位置的 `management.html`：

- `./management.html`
- `./static/management.html`
- `MANAGEMENT_STATIC_PATH`
- `WRITABLE_PATH/static/management.html`

如果你的目录结构比较特殊，也可以显式传 `-dir`。

## 使用

直接运行：

```powershell
./cpa-usage-patch
```

可选参数：

```powershell
./cpa-usage-patch -port 8328 -host 127.0.0.1
./cpa-usage-patch -dir "D:\path\to\cli-proxy-api-dir"
```

推荐启动顺序：

1. 启动 `cli-proxy-api`
2. 启动 `cpa-usage-patch`
3. 打开或刷新管理页
4. 再发起新的真实请求

如果管理页在补丁启动前就已经打开，必须手动刷新一次，让 loader 提前接管页面初始化请求。

## 数据文件

- 统计数据写入补丁所在目录的 `cpa-usage-patch-records.jsonl`
- 只要这个文件和补丁还在，CLIProxyAPI 升级后统计仍可继续累计

## 限制

- 现版 CLIProxyAPI 的内存 queue 只保留大约 1 分钟 usage 记录，所以补丁最好常驻运行。
- 后端开关打开之前的旧请求不会被补算。
- 如果未来 CPAMC 大改 DOM 结构，左侧导航注入逻辑可能需要跟着调整。
- 浏览器和补丁程序必须在同一台机器上，因为注入脚本访问的是 `http://127.0.0.1:<port>`。

## GitHub Actions

仓库内已经包含：

- `.github/workflows/ci.yml`
  - Push / Pull Request 时执行 `go test` 和跨平台快照构建检查
- `.github/workflows/release.yml`
  - 推送标签时通过 GoReleaser 发布 GitHub Release
- `.goreleaser.yml`
  - 平台矩阵、归档格式、校验文件都在这里定义

普通 push 只会触发 CI，不会上传 artifact，也不会自动创建 GitHub Release。

发布方式建议使用语义化标签，例如：

```bash
git tag v0.1.0
git push origin v0.1.0
```

## 本地开发

```powershell
go build .
go test ./...
```
