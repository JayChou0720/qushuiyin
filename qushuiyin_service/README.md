# Go 服务：本地开发

## M1：小程序 Mock 联调

Windows 下，从仓库根目录启动。脚本需要 Go 和 MySQL 8.4（`mysqld.exe` 需在 PATH 中），会启动隔离的开发库（默认端口 33316），执行迁移与开发种子，并在 `http://127.0.0.1:18080` 提供 Mock API 和样本文件。现有 MySQL 不会被改动。

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File ./qushuiyin_service/scripts/start-mock.ps1 -Background
node ./qushuiyin_service/scripts/smoke-mock.mjs
```

在微信开发者工具中打开 `qushuiyin_miniapp/`。首页点「填入示例」可测试视频；粘贴 `https://www.xiaohongshu.com/explore/10002` 可测试图集。结果页的保存按钮下载本地样本到临时文件，当前不写入相册。开发登录和样本只用于本机联调，不访问平台的真实媒体。小程序体验版或正式版不使用本地地址。

## M0：能力验证

以下命令用于解析能力验证和 CDN 主机采集。

在本目录运行（Go 未进入当前终端 PATH 时用 `D:\Go\bin\go.exe`）：

```powershell
go build -o bin/qushuiyin-server.exe ./cmd/qushuiyin-server
$env:APP_ENV = 'local'
$env:PARSE_MODE = 'mock'
.\bin\qushuiyin-server.exe probe 'https://www.xiaohongshu.com/explore/10002'
go vet ./...
go test ./...
```

Mock 只回放合成解析结果与媒体响应头，不访问网络、不写数据库，也不代表平台可用。用真实样本时设 `PARSE_MODE=live`，保持 `PARSE_VIDEO_DOUYIN_COOKIE` 和 `MOCK_MEDIA_ORIGIN` 未设置；用单引号把完整分享 URL 作为一个参数传给 `probe`。命令只输出作品 ID、类型、资源数量、最终主机、MIME、大小和判定原因。不要把样本 URL 或终端命令历史放进报告。

不配置 `MYSQL_DSN` 也可采集主机，但状态为 `untracked`。要写入本地开发库，请先设 `MYSQL_DSN`（只在本机设置），对 `qushuiyin_dev` 运行 `qushuiyin-server migrate up`；用 `migrate status` 查看迁移状态。探测只会将新主机记为 `observed`，重复观测不会覆盖现有登记状态。直连判定是服务端探测结果，不是相册保存或真机域名校验的结论。

真实样本文件参照 `testdata/live_samples.example.json`，置于 `testdata/live_samples.local.json`（已被 Git 忽略）。正例填 `platform` 和 `mediaType`；反例填预期 `error`。运行 `go test -tags live -run TestLiveSamples -v ./cmd/qushuiyin-server`。MySQL 集成测试使用单独的 `MYSQL_TEST_DSN`，只在 `qushuiyin_test` 库运行。未配置样本或测试连接时相应测试会跳过。

用 [M0 能力验证表](doc/m0-validation.md) 汇总实测结果。
