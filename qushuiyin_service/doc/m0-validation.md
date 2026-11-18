# M0 能力验证表

已完成四平台首轮真实探测。抖音的 JPEG 优先复测使用 `douyin-h5-iteminfo-v2`，iPhone Safari 短链复测使用 `douyin-h5-iteminfo-v3`。其余平台用解析库提交 `00e8e118c7c52e287b6b06858639ee86aab0579c`。**抖音短链稳定性、授权样本矩阵和真机验证待完成**。

| 平台 | 视频 | 静态图集 | 封面 | 文案 |
| --- | --- | --- | --- | --- |
| 抖音 | 本地样本探测到 MP4，域名未登记 | 两组共 27 张正文图均探测到 JPEG，域名未登记 | 视频和图集样本均探测到 WebP，下载准备待验证 | H5 映射通过离线测试 |
| 小红书 | 本地样本探测到 MP4，域名未登记 | 本地样本 6 张正文图均探测到 JPEG，部分探测有波动 | 视频和图集的封面均探测到 JPEG，域名未登记 | 待验证 |
| 快手 | 待验证 | 待验证 | 待验证 | 待验证 |
| B 站 | 第 1P 通过魔数识别为 MP4，按 `relay_only` 处理 | 首版不支持 | 第 1P 探测到 PNG，域名未登记 | 待验证（标题） |

每个拟开放的“平台 × 内容类型”按 T02 收集至少 20 个有权使用的样本。B 站另测多 P 视频：未指定 P 和 `p=1` 应尝试解析，`p=2` 应拒绝。图集记录 `imagesAndVideo` 是否同时为真；实况图记录是否正确降级。

| 日期 / 环境 | 平台 / 内容类型 | 样本与结果 | 失败类别 | 解析器版本 | iOS / Android 客户端版本 |
| --- | --- | --- | --- | --- | --- |
| 2026-11-18 / Windows CLI | B 站 / 开放许可多 P 视频 | 1 个作品的第 1P 返回视频和封面，第 2P 按预期拒绝 | 视频的 `application/octet-stream` 未通过直连探测。封面升级 HTTPS 后通过图片探测，域名未登记 | 锁定解析库 | 未测 |
| 2026-11-18 / Windows CLI | 快手 / 视频 | 1 / 1，仅探测 | 域名未登记 | 锁定解析库 | 未测 |
| 2026-11-18 / Windows CLI | 抖音 / 视频 | 3 / 0，仅探测 | `UPSTREAM_UNAVAILABLE` | 锁定解析库 | 未测 |
| 2026-11-18 / Windows CLI | 小红书 / 内容类型未确认 | 1 / 0，仅探测 | `UPSTREAM_UNAVAILABLE` | 锁定解析库 | 未测 |
| 2026-11-18 / Windows CLI | 抖音 / 视频（DY-V1 至 V3） | 3 条中 2 条返回 MP4 和封面 | 1 条 `UPSTREAM_UNAVAILABLE`。MP4 域名未登记，两个 WebP 封面均为 `UNSUPPORTED_MIME` | `douyin-h5-iteminfo-v1` | 未测 |
| 2026-11-18 / Windows CLI | 抖音 / 图集候选（DY-G1 至 G3） | 3 条中 1 条解析出 9 张图。`imagesAndVideo=true`，正文使用图集，附独立封面。该样本通过 iteminfo 取得图集 | 2 条 `UPSTREAM_UNAVAILABLE`。9 张图和封面均为 WebP，直连探测为 `UNSUPPORTED_MIME` | `douyin-h5-iteminfo-v1` | 未测 |
| 2026-11-18 / Windows CLI | 小红书 / 内容类型未确认 | 4 条短链、4 条带签名的作品链接，均未取得媒体 | `UPSTREAM_UNAVAILABLE`，尚未进入媒体探测 | 锁定解析库 | 未测 |

抖音第一次复测于北京时间 17:29 与 17:35 各跑一轮。第二轮 DY-V3 仍探测到 MP4，DY-G1 仍返回 9 张图和封面。首轮探测到 MP4 的 DY-V2 在第二轮返回 `UPSTREAM_UNAVAILABLE`。

2026-11-18 北京时间 18:06 至 18:07，Windows CLI 从 `testdata/live_samples.local.json` 读取样本。`TestLiveSamples` 的 8 条断言样本全部通过，且单独运行 `probe` 复测了新样本。样本编号对应本地文件中的顺序。

| 样本 | 探测结果 | 限制与波动 |
| --- | --- | --- |
| 5 小红书图集 | `probe` 最后一次和通过的 live 测试均探测到 6 张 JPEG，另有 1 张 JPEG 封面。`imagesAndVideo=false` | 前几轮部分正文收到 `206` 和 `Content-Range`，但未读到文件头，记为 `UNSUPPORTED_MIME`。域名未登记 |
| 6 小红书视频 | 探测到 MP4 和 JPEG 封面 | 早期运行中封面返回 `application/octet-stream`，有一次未识别。域名未登记 |
| 7 抖音视频 | 探测到 1 个 MP4 和 WebP 封面 | 曾返回 `UPSTREAM_UNAVAILABLE`。MP4 域名未登记，WebP 封面需走转换 |
| 8 抖音图集 | 探测到 18 张 JPEG 和 WebP 封面。`imagesAndVideo=true` | 短链终点曾返回 HEAD 444，但已取得作品 ID。独立的 H5 iteminfo 成功运行，也曾出现 `UPSTREAM_UNAVAILABLE`。JPEG 域名未登记 |
| 12 抖音图集（DY-G1） | 与上一版相同作品的 9 张正文图由 WebP 改选为 JPEG，封面仍为 WebP。`imagesAndVideo=true` | JPEG 域名未登记，WebP 封面需走转换 |
| 3 B 站第 1P | 响应声明 `application/octet-stream`，读文件头识别为 MP4。封面仍为 PNG | 视频按 `RELAY_ONLY` 处理，中转待测 |

2026-11-18 北京时间 18:27 至 18:28，Windows CLI 用 `douyin-h5-iteminfo-v3` 对同一文件的两条抖音短链各连续执行 12 次 `probe`。

| 样本 | 解析成功 | `UPSTREAM_UNAVAILABLE` |
| --- | --- | --- |
| 7 抖音视频短链 | 0 / 12 | 12 |
| 8 抖音图集短链 | 0 / 12 | 12 |

诊断中两条短链均取得作品 ID。上游以 HTTP 200 返回验证页面，缺少 `xsstoken` 与 `webId`，重新请求一次仍缺字段。北京时间 18:30 对两条短链各补跑一次，结果相同。本轮稳定性验收未通过。

| 最终 CDN 主机 | 平台 | 观测数 / MIME | 登记状态 | iOS / Android 下载、预览与保存结论 |
| --- | --- | --- | --- | --- |
| tymov2.a.kwimgs.com | 快手 | 1 / video/mp4（56,703,677 字节） | 未登记 | 未测 |
| p2.a.yximgs.com | 快手 | 1 / image/jpeg（28,197 字节） | 未登记 | 未测 |
| upos-sz-estgcos.bilivideo.com | B 站 | 1 / application/octet-stream（74,150,122 字节） | 未登记 | 未测 |
| i2.hdslb.com | B 站 | 1 / image/png（23,044 字节） | 未登记 | 未测 |
| v11-cold-src.douyinvod.com | 抖音 | 1 / video/mp4（4,701,582 字节） | 未登记 | 未测 |
| v95-hzyy-thr-daily-colda.douyinvod.com | 抖音 | 1 / video/mp4（309,393 字节） | 未登记 | 未测 |
| p26-sign.douyinpic.com | 抖音 | 9 / image/webp（7 张正文，2 张封面） | 未登记 | 直连探测拒绝，转换与真机保存待测 |
| p11-sign.douyinpic.com | 抖音 | 3 / image/webp（2 张正文，1 张封面） | 未登记 | 直连探测拒绝，转换与真机保存待测 |

最新一轮观测到的新增主机和 JPEG 数量如下。域名均未登记，iOS 与 Android 下载、预览和相册保存均未测。

| 最终 CDN 主机 | 平台 | 格式与观测 |
| --- | --- | --- |
| v5-se-bd-daily-cold.douyinvod.com | 抖音 | 1 个 MP4 |
| p5-ex-gddgtc-sign.douyinpic.com | 抖音 | 6 张正文 JPEG |
| p9-sign.douyinpic.com | 抖音 | 3 张正文 JPEG |
| p11-sign.douyinpic.com | 抖音 | 4 张正文 JPEG，1 张 WebP 封面 |
| p26-sign.douyinpic.com | 抖音 | 14 张正文 JPEG，2 张 WebP 封面 |
| sns-webpic-qc.xhscdn.com | 小红书 | 6 张正文 JPEG，2 张 JPEG 封面 |
| sns-video-v4.xhscdn.com、sns-video-v3.xhscdn.com | 小红书 | 同一视频在不同轮次探测到 MP4 |
| upos-sz-estgcos.bilivideo.com、upos-sz-mirrorcos.bilivideo.com | B 站 | 同一视频在不同轮次通过魔数识别为 MP4 |

B 站样本是 Blender Foundation 的 *Big Buck Bunny*（Peach 项目许可 CC BY 3.0）。其余探测使用本地样本及公开复现链接。探测只访问元数据和响应头，必要时最多读取 12 字节文件头。这些探测尚未计入授权下载样本数或平台成功率。历史复现链接已补进本地文件，标记 `probeOnly` 的链接在 live 测试中保留观测结果。

汇总只写样本编号和最终主机，不写源 URL、签名或 Cookie。真机验证需开启正式域名校验。确认前保持能力和直连预览关闭；B 站视频继续按 `relay_only` 验证。
