# Go 服务：本地开发

## M1：小程序 Mock 联调

Windows 下，从仓库根目录启动。脚本需要 Go 和 MySQL 8.4（`mysqld.exe` 需在 PATH 中），会启动隔离的开发库（默认端口 33316），执行迁移与开发种子，并在 `http://127.0.0.1:18080` 提供 Mock API 和样本文件。现有 MySQL 不会被改动。

```powershell
powershell -NoProfile -ExecutionPolicy Bypass -File ./qushuiyin_service/scripts/start-mock.ps1 -Background
node ./qushuiyin_service/scripts/smoke-mock.mjs
```

在微信开发者工具中打开 `qushuiyin_miniapp/`。首页点「填入示例」可测试视频，粘贴 `https://www.xiaohongshu.com/explore/10004` 可测试三张图集。结果页点击保存后会检查隐私授权和相册权限，通过后向相册保存样本。当前客户端联调入口限开发版。

结果页点「播放视频」或图集的「预览」可检查样本预览。以下链接仅用作 Mock 输入，不会访问真实媒体：

| 输入 | 预期保存结果 |
| --- | --- |
| `https://www.douyin.com/video/10004` | 首次准备超时；点「重试未完成项」后下载成功。 |
| `https://www.douyin.com/video/10005` | 模拟直连被拒，自动切换中转并下载成功。 |
| `https://www.douyin.com/video/10006` | 模拟直连被拒且无法中转，提供短期 Mock 下载链接。 |

如果开发者工具提示回环地址不在合法域名中，用独立的本地调试副本，并在副本的 `project.private.config.json` 设置 `setting.urlCheck=false`。不要提交该设置或用它验证线上域名。默认端口占用时，接口测试可用 `start-mock.ps1 -Background -ApiPort 18082` 和 `node ./qushuiyin_service/scripts/smoke-mock.mjs 18082`；小程序本地地址仍为默认的 18080。

管理员账号与小程序账号分开。设置本机 `MYSQL_DSN` 并迁移后，用 `qushuiyin-server admin create --username ops --role super_admin` 创建首个管理员；终端会要求两次不回显的密码输入。管理会话仅通过 `/admin` 路径的安全 Cookie 签发，修改请求校验 `Origin`。管理员接口的 MySQL 集成测试使用 `MYSQL_TEST_DSN`，只对本地 `qushuiyin_test` 运行。

客服、运营和超级管理员可筛选 `GET /admin/v1/feedbacks?status=pending&category=download_failed`，或访问工单详情。修改工单提交 `{expectedVersion,status?,reply?,reason}`，原因不要填写用户的链接或联系方式。工单关闭后按配置缩短保留期；仅超级管理员和审计角色可用 `GET /admin/v1/audit-logs?targetType=feedback&targetId=...` 查看脱敏审计。`go test -run TestAdminFeedbackMySQL ./internal/httpapi` 验证接口权限与回复流程。

同一个接口脚本还使用独立的开发身份验证反馈、事件和数据清理。反馈链接只在明确附带时接受；反馈与事件写入本地 MySQL。Mock 事件暂用以下值：`stage` 为 `result/preview/save/download/album`，`outcome` 为 `started/succeeded/failed/cancelled/partial/unknown`，`osFamily` 为 `ios/android/windows/macos/devtools/unknown`。重复事件按用户与事件 ID 判定；本人失败解析可用 `error.traceId` 关联反馈。

在模拟器中，从「使用帮助」进入「提交反馈」或「我的反馈」。提交测试反馈后会显示工单编号；通过本地管理接口回复该工单，再打开「我的反馈」，可核对状态、最新回复和回复时间。附带作品链接默认未勾选，取消勾选会清除已输入的链接。

清理请求撤销本地开发用户的会话和票据，并记录可在重启后继续处理的任务；共用的合成样本不受影响。`go test -run TestMockPrivacyMySQL ./internal/httpapi` 使用隔离测试库验证删除边界。该测试不覆盖真实媒体文件和备份恢复。

首页或「使用帮助」可以打开「数据与隐私」。「清理本机缓存」清理小程序跟踪的临时下载和缩略图，保留记录文字；「清除服务端数据」需要确认，并清除当前身份的本机记录。使用独立测试身份复测时，先解析示例或提交测试反馈，再发起清除。成功后小程序回到首页，显示访问已停止和在线副本的计划清理期限；旧反馈和本服务签发的媒体票据均不能再访问。

连续解析同一示例两次，返回首页打开「最近记录」，应只有一条记录。打开记录应显示结果；本机结果已过期时，会用原分享链接重新解析。在「数据与隐私」关闭记录开关并选择保留，再解析另一示例，列表应保持原样。「清空记录」会移除当前身份的记录及缩略图，不删除相册里的文件。

启动 Mock 后，在 `qushuiyin_miniapp` 下用已安装的 TypeScript 编译器和 Node 运行最近记录回归。脚本使用独立开发身份和模拟的微信本机存储；自定义端口时替换末尾的 `18080`。

```powershell
$historyBuild = Join-Path $env:TEMP ('qsy-history-' + [guid]::NewGuid().ToString('N'))
npx tsc --noEmit -p .
npx tsc --outDir $historyBuild -p .
node ./scripts/smoke-history.cjs $historyBuild 18080
node ./scripts/smoke-events.cjs $historyBuild 18080
```

事件回归复用相同的临时编译目录，覆盖离线暂存、批量补传、回执丢失后的退避和去重，以及清除当前身份的待传队列。脚本使用模拟的微信相册回调验证图集选择、逐项保存、部分失败、已下载文件复用、取消和恢复。iOS 和 Android 的实际相册写入及权限流程由真机验收确认。

## M2：持久化与本地真实中转

`serve` 的任务、结果、候选源、批次和幂等记录使用 MySQL，重启后由过期租约恢复。启动前执行 `migrate up`，保留同一套 `DATA_ENCRYPTION_KEYS`、`HMAC_KEY` 和 `TICKET_SIGNING_KEY`。`start-mock.ps1` 会在项目外的本机运行目录保存开发密钥，勿删除后用新密钥连接旧数据库。

本地真实解析与中转使用 `APP_ENV=local`、`PARSE_MODE=live`、`WECHAT_DEV_LOGIN=true`，设置回环 `HTTP_ADDR`，清除 `MOCK_MEDIA_ORIGIN`。此外设置绝对路径 `MEDIA_DIR` 与 `FFPROBE_PATH`，例如 `D:\FFmpeg\bin\ffprobe.exe`，再运行 `qushuiyin-server serve`。只有开发、测试环境可运行这一入口。真实微信登录与生产入口尚未开放。

媒体按平台策略优先直连已登记主机，否则共享整批重新解析预算，再尝试中转或提供本人复制链接。地址加密保存，fallback 只允许一次且幂等；主机达到回退阈值后转 review。中转通过实际 GET、字节数、魔数及 ffprobe 校验；下载支持 HEAD 和单段 Range。JPEG/PNG 做完整解码和像素限制；静态 WebP 按透明通道转为 JPEG/PNG，并持久化 converted 标记。动态 WebP 返回 FORMAT_UNSUPPORTED，不保存首帧。

预览按需申请 preview-tickets，视频输出受限 Range，图片第一次请求才生成共享 JPEG 缩略图；放大走独立用途签名的大图接口。预览共用连接保护与 MySQL 出站额度，票据还限制累计字节。direct_preview_enabled 默认 false，真机验证后才开启。测试使用录制响应，不连续请求真实平台。

图片解码、缩放和 WebP 转换共用 image_convert_workers，解码前检查 max_image_pixels，转换输出仍受 max_image_bytes 约束。WebP JPEG 质量暂取 thumbnail_jpeg_quality，见根目录 Q12。图片测试使用提交的合成小图，无需额外安装 FFmpeg；视频验证仍需要 ffprobe。

MySQL 验证用 `MYSQL_TEST_DSN` 指向独立的回环 `qushuiyin_test` 库。设置 `FFPROBE_TEST_PATH` 后运行 `go test -p 1 ./...`，可执行任务恢复、幂等、取消竞争、旧媒体租约回写、文件复用、Range 和清理测试。`-p 1` 防止多个包同时迁移同一个测试库。

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

真实样本文件参照 `testdata/live_samples.example.json`，置于 `testdata/live_samples.local.json`（已被 Git 忽略）。正例填 `platform` 和 `mediaType`；反例填预期 `error`。运行 `go test -tags live -run TestLiveSamples -v ./cmd/qushuiyin-server`。MySQL 集成测试使用单独的 `MYSQL_TEST_DSN`，只在 `qushuiyin_test` 库运行。未配置样本或测试连接时相应测试会跳过。真实样本回归同一平台的样本间隔至少 10 秒，单轮最多 10 条。遇到验证页会停止该平台的样本测试。手动运行 `probe` 同样遵守这些间隔，遇到验证页至少等待 30 分钟再测。

用 [M0 能力验证表](doc/m0-validation.md) 汇总实测结果。
