# Go 服务：M0 能力验证

目前的命令用于解析能力验证和 CDN 主机采集。平台能力仍待实测确认，小程序和业务 HTTP 服务不在本阶段启动。

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
