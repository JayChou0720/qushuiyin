# M0 能力验证表

已完成四平台首轮真实探测，以及抖音和小红书复测。**授权样本矩阵和真机验证待完成**。抖音复测用 `douyin-h5-iteminfo-v1`，其余平台用解析库提交 `00e8e118c7c52e287b6b06858639ee86aab0579c`。

| 平台 | 视频 | 静态图集 | 封面 | 文案 |
| --- | --- | --- | --- | --- |
| 抖音 | 两个短链样本曾探测到 MP4，域名未登记，其中一条后续复跑失败 | 一个 H5 样本解析出 9 张静态图，均为 WebP，下载准备待验证 | 视频和图集样本均探测到 WebP，下载准备待验证 | H5 映射通过离线测试 |
| 小红书 | 复测未取得媒体 | 复测未取得媒体 | 待验证 | 待验证 |
| 快手 | 待验证 | 待验证 | 待验证 | 待验证 |
| B 站 | 待验证，仅第 1P 和单文件 MP4 | 首版不支持 | 待验证 | 待验证（标题） |

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

抖音视频和图集于北京时间 17:29 与 17:35 各跑一轮。第二轮 DY-V3 仍探测到 MP4，DY-G1 仍返回 9 张图和封面。首轮探测到 MP4 的 DY-V2 在第二轮返回 `UPSTREAM_UNAVAILABLE`。

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

离线回归验证了视频、图集和封面候选的 HTTP → HTTPS 改写，主机、路径及原始签名参数保持不变。小红书短链和带签名的作品链接各抽查一条，锁定解析库均未能从返回页面提取笔记数据。本批次没有小红书 CDN 可访问性结论。

B 站样本是 Blender Foundation 的 *Big Buck Bunny*（Peach 项目许可：CC BY 3.0）。其余探测只用了公开技术报告或项目文档中用于复现的链接，只读取元数据与响应头，未下载或保存媒体。这些探测不计入有授权的下载样本数，也不用于推算平台成功率。

汇总只写样本编号和最终主机，不写源 URL、签名或 Cookie。真机验证需开启正式域名校验。确认前保持能力和直连预览关闭；B 站视频继续按 `relay_only` 验证。
