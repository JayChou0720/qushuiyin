# Go 服务：本地开发

## M1：小程序 Mock 联调

Windows 下，从仓库根目录启动。脚本需要 Go 和 MySQL 8.4（`mysqld.exe` 需在 PATH 中），会启动隔离的开发库（默认端口 33316），执行迁移与开发种子，并在 `http://127.0.0.1:18080` 提供 Mock API 和样本文件。现有 MySQL 不会被改动。

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File ./qushuiyin_service/scripts/start-mock.ps1 -Background
node ./qushuiyin_service/scripts/smoke-mock.mjs
```

在微信开发者工具中打开 `qushuiyin_miniapp/`。首页点「填入示例」可测试视频；粘贴 `https://www.xiaohongshu.com/explore/10002` 可测试图集。结果页的保存按钮下载本地样本到临时文件，当前不写入相册。开发登录和样本只用于本机联调，不访问平台的真实媒体。小程序体验版或正式版不使用本地地址。

结果页点「播放视频」或图集的「预览」可检查样本预览。以下链接仅用作 Mock 输入，不会访问真实媒体：

| 输入 | 预期保存结果 |
| --- | --- |
| `https://www.douyin.com/video/10004` | 首次准备超时；点「重试未完成项」后下载成功。 |
| `https://www.douyin.com/video/10005` | 模拟直连被拒，自动切换中转并下载成功。 |
| `https://www.douyin.com/video/10006` | 模拟直连被拒且无法中转，提供短期 Mock 下载链接。 |

如果开发者工具提示回环地址不在合法域名中，用独立的本地调试副本，并在副本的 `project.private.config.json` 设置 `setting.urlCheck=false`。不要提交该设置或用它验证线上域名。默认端口占用时，接口测试可用 `start-mock.ps1 -Background -ApiPort 18082` 和 `node ./qushuiyin_service/scripts/smoke-mock.mjs 18082`；小程序本地地址仍为默认的 18080。

管理员账号与小程序账号分开。设置本机 `MYSQL_DSN` 并迁移后，用 `qushuiyin-server admin create --username ops --role super_admin` 创建首个管理员；终端会要求两次不回显的密码输入。管理会话仅通过 `/admin` 路径的安全 Cookie 签发，修改请求校验 `Origin`。管理员接口的 MySQL 集成测试使用 `MYSQL_TEST_DSN`，只对本地 `qushuiyin_test` 运行。

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
