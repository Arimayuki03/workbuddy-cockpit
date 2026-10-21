# NanoPi R28S 部署手册（FriendlyWrt / OpenWrt ARM64）

> 通用部署示例，不包含个人地址、账号清单、排程或私有运维记录。
> 更新日期：2026-10-22；对应发行版：v1.20.0。

## 1. 示例环境

适用于 NanoPi R28S 等 ARM64 小主机、FriendlyWrt / OpenWrt 和支持 `linux/arm64` 的 Docker。示例使用单容器 `workbuddy2api`、host 网络和 `unless-stopped` 重启策略。

网关与面板共用 7863；如自行安装管理桥，可额外使用内网 17863。可执行文件和 DNS 配置只读挂载，仅 `config`、`auths`、`data` 可写。

## 2. 目录与挂载

| 宿主机示例路径 | 容器路径 | 模式 | 用途 |
|---|---|---|---|
| `/opt/wb2api/bin/wb2api-low-write` | `/app/wb2api` | ro | ARM64 主程序 |
| `/opt/wb2api/config` | `/app/config` | rw | 配置，包含敏感值 |
| `/opt/wb2api/auths` | `/app/auths` | rw | 账号凭据 |
| `/opt/wb2api/data` | `/app/data` | rw | 状态、用量、积分流水及密钥摘要 |
| `/opt/wb2api/resolv.conf` | `/etc/resolv.conf` | ro | 按实际网络准备的 DNS 配置 |

`config` 必须挂目录：面板热改配置使用临时文件 + rename，单文件 bind mount 会阻止原子替换。示例以 root 运行，`auths`、`data` 目录使用 0700，配置与凭据文件使用 0600；如改用非 root，同步设置属主和读写权限。

## 3. 构建或下载

可以下载同一 Release 的 `wb2api-v1.20.0-linux-arm64`，按 `sha256sums-v1.20.0.txt` 校验后重命名为 `wb2api-low-write`。也可在开发机交叉编译：

```bash
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -tags embed_panel -trimpath \
  -ldflags="-s -w -X workbuddy2api/internal/server.appVersion=v1.20.0" \
  -o wb2api-low-write ./cmd/server
```

`internal/panel/dist` 必须存在并与前端源码一致。修改前端时先按 README 构建并同步静态产物；不建议在内存有限的路由器上运行 Next.js 构建。务必注入 `appVersion`，否则面板与版本检查会显示 `dev`。

示例的 `workbuddy2api:router` 是自行准备的 ARM64 运行时镜像，需提供程序所需的 CA 证书等运行文件，不是发行镜像名称。也可在开发机用仓库 Dockerfile + buildx 构建 `linux/arm64`。不得将个人配置、凭据或业务数据打入镜像。

## 4. 配置与容器

复制 `config.example.json` 到 `/opt/wb2api/config/config.json`，按挂载调整 `auth_dir`、`state_file` 及其他持久化路径，设置随机强 `api_key`。下面仅为关键字段示例，不能用片段覆盖已有完整配置：

```jsonc
{
  "listen": ":7863",
  "api_key": "<替换为随机强密钥>",
  "auth_dir": "/app/auths",
  "state_file": "/app/data/state.json",
  "panel": { "enabled": true },
  "admin": { "enabled": true },
  "reasoning_as_content": false,
  "schedule": {
    "task_sweep_enabled": false,
    "task_sweep_interval_minutes": 15
  }
}
```

新增开关默认关闭。`reasoning_as_content` 改动需重启；奖励补扫只领取已达标奖励，周期最小 15 分钟。开启 panel 或 admin 时 `api_key` 为空会拒绝启动。

准备好路径、程序权限及 DNS 文件后，示例启动命令为：

```bash
docker run -d --name workbuddy2api \
  --restart unless-stopped --network host \
  --log-opt max-size=10m --log-opt max-file=3 \
  -e TZ=Asia/Shanghai \
  -v /opt/wb2api/config:/app/config \
  -v /opt/wb2api/auths:/app/auths \
  -v /opt/wb2api/data:/app/data \
  -v /opt/wb2api/bin/wb2api-low-write:/app/wb2api:ro \
  -v /opt/wb2api/resolv.conf:/etc/resolv.conf:ro \
  workbuddy2api:router /app/wb2api -config /app/config/config.json
```

host 网络下不使用 `-p`，可达性由宿主防火墙和 VPN ACL 决定。示例日志轮转只有创建/重建容器才生效，不能据文档推断现有容器已配置。

## 5. 可选内网管理桥

`wb2api-admin-proxy` 不是本仓库的发行资产。如需 17863 入口，自行提供该程序和 procd 服务，监听 `:17863` 并转发到 `127.0.0.1:7863`。没有安装时跳过本节及相关验收项。

管理桥不增加鉴权，仍由网关 `api_key` 负责。17863 仅向可信管理设备开放，不允许公网或共享入口放行；不要更改 SSH、防火墙或其他服务来套用此示例。

## 6. 升级与停服备份

先在开发机构建或下载程序，再上传临时文件，比较本地与远端 SHA-256。SSH 严格校验已知主机密钥，不关闭 known_hosts 检查：

```bash
scp wb2api-low-write root@<R28S>:/opt/wb2api/bin/wb2api-low-write.new
ssh root@<R28S> 'sha256sum /opt/wb2api/bin/wb2api-low-write.new'
```

记录原容器、挂载、版本、配置哈希和账号池，等待在途请求结束。以下命令在设备上执行，备份目录自行替换；备份包含凭据，不得公开上传：

```bash
umask 077
BACKUP_DIR="/root/maintenance-backups/workbuddy-$(date +%Y%m%d-%H%M%S)"
mkdir -p "$BACKUP_DIR"
chmod 700 "$BACKUP_DIR"
cp -p /opt/wb2api/bin/wb2api-low-write "$BACKUP_DIR/wb2api-low-write.old"
docker inspect workbuddy2api > "$BACKUP_DIR/container-inspect.json"
docker stop --time 30 workbuddy2api
tar -czf "$BACKUP_DIR/persistent-data.tar.gz" -C /opt/wb2api config auths data
tar -tzf "$BACKUP_DIR/persistent-data.tar.gz" > /dev/null
sha256sum "$BACKUP_DIR/wb2api-low-write.old" "$BACKUP_DIR/persistent-data.tar.gz" \
  > "$BACKUP_DIR/sha256sums.txt"
```

逐项确认成功后再继续；任一步失败先启动原容器恢复服务并排查。归档还应在受限临时目录做解压读取验证。`docker inspect` 也可能含敏感环境变量，仅保存在受限备份目录。

确认上传校验一致及备份可用后，在停服状态原子替换，保留配置和挂载：

```bash
chmod 755 /opt/wb2api/bin/wb2api-low-write.new
mv /opt/wb2api/bin/wb2api-low-write.new /opt/wb2api/bin/wb2api-low-write
docker start workbuddy2api
```

二进制是文件 bind mount，替换宿主机文件后必须重新启动容器以重新挂载；不使用 `docker cp` 覆盖只读挂载。Docker 返回的挂载列表顺序可能变化，比较完整定义时应按容器目标路径排序。

## 7. 验收

```bash
docker inspect --format '{{.State.Status}} {{.HostConfig.RestartPolicy.Name}}' workbuddy2api
wget -qO- http://127.0.0.1:7863/healthz
PID=$(docker inspect --format '{{.State.Pid}}' workbuddy2api)
sha256sum /opt/wb2api/bin/wb2api-low-write "/proc/$PID/exe"
```

- 确认运行中程序哈希与上传产物一致，面板版本为 v1.20.0。
- 面板根路径、账号页和其新版静态资源正常，配置哈希与账号集合符合升级前记录。
- 未鉴权访问受保护 API 返回 401；用管理员凭据发送空搜索请求应为 400，确认新路由参数校验已生效。凭据不得写进日志或公开命令。
- 如安装管理桥，额外检查 `http://127.0.0.1:17863/healthz`。
- 从实际 LAN / VPN 入口验证，不能仅凭容器为 Up 判断全部正常。
- 日志无持续 panic 或异常重启，持久化目录可读写；健康检查不等于真实模型对话、搜索或奖励领取已验证。

## 8. 回滚与数据恢复

启动或验收失败时，使用原程序回退。`BACKUP_DIR` 必须指向已核验的本次备份目录：

```bash
cp -p "$BACKUP_DIR/wb2api-low-write.old" /opt/wb2api/bin/wb2api-low-write.rollback
chmod 755 /opt/wb2api/bin/wb2api-low-write.rollback
docker stop --time 30 workbuddy2api
mv /opt/wb2api/bin/wb2api-low-write.rollback /opt/wb2api/bin/wb2api-low-write
docker start workbuddy2api
```

这里只回退程序，保留当前配置、账号和业务数据。恢复持久化快照是另一个操作：先停止容器并另存当前数据，核对备份时间、归档与校验和，再恢复需要的数据，避免用旧快照覆盖升级后的新增状态。不能把二进制或镜像备份当作业务数据备份。

## 9. 低写入与安全

- `usage.json` 有防抖原子写；状态和用量仍需可靠持久化，不把整个 `data` 目录搬到易失 tmpfs。容量按账号数、流量和保留策略监测。
- 对 Docker 日志设置大小和轮转数量，避免 SD 卡长期承受无上限写入；前端构建放在开发机。
- 自行检查防火墙与 VPN ACL，不能假设 WAN 已拒绝访问。7863 同时承载业务与面板；端口放行不等于只放行业务 API。
- 公网访问需 HTTPS 反代并正确声明可信代理网段；VPN 隧道加密不等于浏览器 HTTPS。
- 不提交配置、凭据、备份、设备地址、UID、昵称、用量记录或未经脱敏的 inspect。发布文件只包含程序、打包归档和校验和。

## 10. 常见问题

| 现象 | 检查方向 |
|---|---|
| 面板显示 dev | 重新构建并注入 `appVersion`，确认替换的是运行中程序 |
| 账号数为 0 | `auth_dir`、目录挂载、容器用户和凭据权限 |
| 热改配置不能持久化 | config 是否误挂为单文件，是否有写权限 |
| 上游 DNS 失败或超时 | 宿主与容器 DNS 是否适合实际网络，resolv.conf 是否有效 |
| 账号因 401 HTML 禁用 | 上游会话被吊销，需重新扫码或授权登录；不能简单重启掩盖 |
| SD 卡日志占用增长 | 检查 Docker json-file 轮转及应用日志保留策略 |
