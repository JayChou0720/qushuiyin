# 单机发布与回滚

以《02》§19 的 2 核 2 GB、50 GB SSD 作为测量起点。Go、Nginx 和 MySQL 同机，Go 仅监听回环地址。请在发布前核对可用磁盘、实际出站带宽和 2 GiB 交换区。

## 构建和版本记录

从仓库根目录构建 Vue，再编译 Go。下面以 Linux amd64 为例，目标架构以部署主机为准。

```sh
npm --prefix qushuiyin_service/web/admin ci
npm --prefix qushuiyin_service/web/admin run build
cd qushuiyin_service
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -o bin/qushuiyin-server ./cmd/qushuiyin-server
go version -m bin/qushuiyin-server
sha256sum bin/qushuiyin-server
```

将二进制和 [parse-video 许可](licenses/parse-video-MIT.txt) 放在 `/opt/qushuiyin/releases/<version>/`，由 root 管理写权限。记录 Git SHA、解析器 SHA、二进制校验和、管理端构建和小程序版本。每个发布版本保留独立目录，`current` 链接指向当前制品。Git 中仅保留嵌入目录的占位首页。

## 安装

1. 在目标 Linux 主机创建无登录 shell 的 `qushuiyin` 系统用户及组，并安装 MySQL 8.4、Nginx 和 `ffprobe`。应用以单实例运行。
2. 把 [MySQL 调优模板](mysql/qushuiyin-2c2g.cnf) 合并到发行版读取的 `mysqld` 配置目录。用 `mysqld --validate-config` 校验配置再重启。检查回环监听、buffer pool、连接上限和实际内存。例值对应本项目同机部署的 §19.2。 [MySQL 8.4 配置校验](https://dev.mysql.com/doc/refman/8.4/en/server-configuration-validation.html)
3. 创建独立的生产库，运行账号只需该库的 `SELECT, INSERT, UPDATE, DELETE`。发布时由独立迁移账号执行 `qushuiyin-server migrate up` 和 `migrate status`。通过受控环境提供迁移账号的 `MYSQL_DSN`，运行环境仍使用应用账号。完成后从有交互终端运行 `qushuiyin-server admin create --username <名称> --role super_admin`。
4. 将 [环境模板](env.example) 复制到 `/etc/qushuiyin/production.env`，设置 root 所有、`0600`。填写三个域名、数据库连接、微信凭证及独立随机密钥。确认 `MEDIA_DIR` 在受控状态目录内，外置媒体卷可通过 systemd drop-in 添加对应的 `ReadWritePaths`。同机 Nginx 只信任实际回环代理地址。
5. 安装 [systemd 单元](systemd/qushuiyin.service)，执行 `systemd-analyze verify /etc/systemd/system/qushuiyin.service`、`systemctl daemon-reload`、`systemctl enable --now qushuiyin`。检查 `curl --fail http://127.0.0.1:8080/healthz` 返回 200，数据库不可用时返回 503。健康检查保留在回环入口。 [systemd 状态目录与进程约束](https://www.freedesktop.org/software/systemd/man/latest/systemd.exec.html)
6. 将 [Nginx 站点](nginx/qushuiyin.conf) 放到 `http` 级别的 include，将 [代理片段](nginx/qushuiyin-proxy.conf) 安装到对应 snippets 路径。替换域名、证书和管理网段，检查默认 HTTPS 站点唯一。执行 `nginx -t` 后 reload。应用与入口的请求体、API 和媒体超时与 §16 对齐。外部 Nginx 用直接对端覆盖 `X-Forwarded-For` 与 `X-Real-IP`。如前面另有受信代理，先配置并验证边缘真实地址，再转给应用。 [Nginx 代理配置](https://nginx.org/en/docs/http/ngx_http_proxy_module.html)
7. 小程序体验和正式包填写 `miniprogram/config/env.ts` 中的 `RELEASE_API_ORIGIN`。通过微信后台完成 request 与 downloadFile 合法域名、隐私接口、类目与主体配置。核对正式包使用的 AppID 与服务环境相符。

Nginx 日志格式只记录方法、路径、状态、响应字节和耗时。三个站点把默认错误日志丢弃，避免 Nginx 异常行写入带签名的请求目标。线上诊断使用应用 JSON 日志、健康检查和脱敏的 Nginx 访问计量。需要在隔离环境启用额外诊断时，仅使用合成票据。

## 巡检、压测和告警

本地使用 `qushuiyin_service/scripts/start-mock.ps1` 建独立库，再运行 `node qushuiyin_service/scripts/load-mock.mjs 18080 20`。脚本只访问回环 Mock，测合成视频、图集、Range、回退和失败分支。记录各阶段 p50/p95、实际传输字节、受控拒绝及运行环境。正式带宽、相册和平台能力由单独真机样本验收。

用回环健康检查监测就绪，进程退出由 systemd 重启。将 `journalctl -u qushuiyin -o cat` 中的 JSON `event` 接入已有告警通道，并按事件和平台去重。

| 事件 | 处置 |
| --- | --- |
| `upstream_circuit_open`、`parse_queue_deadline` | 核对对应平台和适配器版本，必要时改为人工维护 |
| `download_domains_unregistered`、`direct_fallback_high`、`download_domain_review` | 在管理端复核主机与样本，确认微信登记后再恢复直连 |
| `media_egress_high`、`media_disk_high_watermark`、`media_storage_unavailable` | 核对额度、卷及缓存。高水位下拒绝新落盘并优先回收无授权且无活动传输的缓存 |
| `media_cleanup_failed`、`media_orphan_removed`、`data_retention_failed`、`user_cleanup_overdue`、`user_cleanup_failed` | 检查磁盘权限、数据库和清理积压。用户删除任务失败后可继续重试 |
| `admin_login_locked`、`operations_check_failed`、`parse_publish_failed` | 检查访问来源、服务和数据库，不在诊断中记录凭证 |
| Nginx 媒体/票据接口错误率与进程就绪失败 | 在现有监控设置状态码分组与基线，确认启动后能收到演练通知 |

备份按 §19.4 滚动。删除清单独立保存，恢复库上线前重放。见 [删除标记恢复流程](../qushuiyin_service/doc/restore-deletions.md)。

## 发布和回滚

先在测试环境运行迁移、管理端嵌入页面、健康检查和 Mock 回归，再走 HTTPS 真机小流量。复核平台初始关闭状态，逐项开放通过样本与真机验收的能力。数据库 00014–00016 新增配置、计数表和索引，可在切换二进制前应用。

异常时先暂停受影响平台或直连选项，保留失败时间、指标和版本。确认旧版可读取当前 schema 后，把 `current` 指回上一制品，再重启服务并核对健康、登录和受影响路径。新表和可兼容迁移保留。若需恢复数据库，隔离恢复并完成签名删除重放，再开放入口。[发布检查表](release-checklist.md)
