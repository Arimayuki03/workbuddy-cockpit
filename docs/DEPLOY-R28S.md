# NanoPi R28S 部署手册（FriendlyWrt / OpenWrt ARM64）

> 本文记录 WorkBuddy Cockpit 在 **NanoPi R28S** 上的实际部署形态，作为 `README.md`「方式四」的展开。
> 所有地址、密钥、账号 UID、昵称均已省略；请按自己的环境替换占位符。
> 记录时间：2026-10-20；对应版本：v1.19.3。

## 1. 环境与现状

| 项 | 现状 |
|---|---|
| 硬件 | NanoPi R28S（ARM64，约 1 GiB RAM） |
| 系统 | FriendlyWrt 25.12.5（OpenWrt 25.12.5，aarch64_generic） |
| 容器运行时 | Docker 27.3.1，overlay2，json-file 日志 |
| 部署形态 | 单容器 `workbuddy2api`，`--network host`，`--restart unless-stopped` |
| 镜像 | 本地构建 `workbuddy2api:router`（linux/arm64，约 18 MB） |
| 应用目录 | `/opt/wb2api`（SD 卡） |
| 监听 | `:7863` 网关 + 面板；`:17863` 内网管理桥 → `127.0.0.1:7863` |
| 账号 | 15 个 cn 账号（2026-10-20 全部健康；以 `/healthz` 实时值为准） |
| 资源占用 | 实测约 52 MiB RSS / 约 5% CPU（采样时仍有请求，仅供参考） |

> 这是**低写入**部署：可执行文件和 `resolv.conf` 只读挂载，只有 `config`、`auths`、`data` 三个目录可写；适用于 SD 卡 / eMMC 寿命敏感的小主机。

## 2. 目录布局与挂载

```text
/opt/wb2api/
├── bin/
│   └── wb2api-low-write      # arm64 主程序，只读挂载到 /app/wb2api
├── config/
│   └── config.json           # 网关配置，目录挂载到 /app/config
├── auths/                    # 账号凭证，目录挂载到 /app/auths
├── data/                     # state / usage / 积分流水，目录挂载到 /app/data
├── resolv.conf               # 只读挂载到 /etc/resolv.conf
└── wb2api-admin-proxy        # 内网管理桥（procd 托管，见 §5）
```

| 宿主机路径 | 容器路径 | 模式 | 用途 |
|---|---|---|---|
| `/opt/wb2api/bin/wb2api-low-write` | `/app/wb2api` | ro | 主程序；升级只替换此文件 |
| `/opt/wb2api/config` | `/app/config` | rw | `config.json`（含 `api_key` 等敏感值） |
| `/opt/wb2api/auths` | `/app/auths` | rw | 每个账号一个 JSON 凭证 |
| `/opt/wb2api/data` | `/app/data` | rw | `state.json` / `usage.json` / 积分流水 / 密钥摘要 |
| `/opt/wb2api/resolv.conf` | `/etc/resolv.conf` | ro | 固定容器 DNS，避免继承宿主不可用的 DNS |

> `config` 必须挂**目录**而不是单个文件：面板/管理端热改配置走 `tmp+rename`，单文件 bind mount 的挂载点会被内核以 `EBUSY` 拒绝。项目已做原地写回退，但目录挂载才是可靠形态。

## 3. 容器创建

当前容器等价于以下命令（路径、镜像名按实际环境调整）：

```bash
docker run -d --name workbuddy2api \
  --restart unless-stopped \
  --network host \
  -e TZ=Asia/Shanghai \
  -v /opt/wb2api/config:/app/config \
  -v /opt/wb2api/auths:/app/auths \
  -v /opt/wb2api/data:/app/data \
  -v /opt/wb2api/bin/wb2api-low-write:/app/wb2api:ro \
  -v /opt/wb2api/resolv.conf:/etc/resolv.conf:ro \
  workbuddy2api:router /app/wb2api -config /app/config/config.json
```

- 当前容器以 root 运行（未设置 `--user`），`/opt/wb2api/{config,auths,data}` 属主 root；`auths/`、`data/` 目录 0700，`config/` 目录 755，敏感文件 0600。若改为非 root，必须同步 `chown`，否则写 `auths/*.json.tmp` 会 `permission denied`。
- `--network host` 下没有 `-p` 端口映射；7863 / 17863 的可达性由宿主防火墙和 Tailscale ACL 决定。
- `docker logs` 走 json-file；当前未设置 `max-size` / `max-file`。长期运行建议重建容器时加 `--log-opt max-size=10m --log-opt max-file=3`，减少 SD 卡写入（见 §10）。

## 4. 镜像与二进制构建

R28S 内存只有约 1 GiB，不建议在设备上跑 Next.js 前端构建。推荐在 x86 开发机交叉编译 arm64 静态二进制，再传到 R28S：

```bash
cd workbuddy-cockpit
CGO_ENABLED=0 GOOS=linux GOARCH=arm64 \
  go build -tags embed_panel -trimpath \
  -ldflags="-s -w -X workbuddy2api/internal/server.appVersion=v1.19.3" \
  -o wb2api-low-write ./cmd/server
```

> `internal/panel/dist` 必须已由 `web` 构建并存在，否则 `-tags embed_panel` 会编译失败。
> **务必注入 `appVersion`**：面板「版本检查」与 `/api/system/check-update` 的 `current` 取这个值；不注入会显示 `dev`，版本比对失真（当前 R28S 上的旧构建就是这种情况）。

当前镜像 `workbuddy2api:router` 是本地 arm64 运行时镜像，容器命令固定为 `/app/wb2api -config /app/config/config.json`。因为二进制是只读 bind mount，日常升级不需要重建镜像：替换宿主机文件并重启容器即可。

如果需要从零构建镜像，可在 x86 上用仓库 `Dockerfile` + buildx 交叉构建 `linux/arm64`，或做一个只提供 `/app` 目录的最小镜像；**不要把 `config.json`、`auths/`、`data/` 打进镜像**。

## 5. 内网管理桥（17863）

R28S 上另有一个独立的 Go 小程序 `wb2api-admin-proxy`，由 procd 托管：

- 文件：`/opt/wb2api/wb2api-admin-proxy`
- 服务：`/etc/init.d/wb2api-admin-proxy`（`START=99`，已 enable）
- 行为：监听 `:17863`，把请求透传到 `127.0.0.1:7863`

```bash
/etc/init.d/wb2api-admin-proxy enable
/etc/init.d/wb2api-admin-proxy restart
wget -qO- http://127.0.0.1:17863/healthz
```

> 管理桥**不做额外鉴权**，鉴权仍由主服务的 `api_key` 负责。它只应通过 LAN / Tailscale 访问；不要在任何公网入口放行 17863。外部/共享入口只应拿到 7863 业务入口，并且仍受 `api_key`、`security` 段和上游账号策略约束。

## 6. 当前配置口径（脱敏）

```jsonc
{
  "listen": ":7863",
  "api_key": "<你的网关密钥>",
  "auth_dir": "/app/auths",
  "state_file": "/app/data/state.json",
  "panel": { "enabled": true },
  "admin": { "enabled": true, "credit_refresh_min_interval_sec": 600 },
  "region": "cn",
  "pool": { "pick_strategy": "credits_desc" },
  "upstream": { "timeout_seconds": 120 },
  "cooldown": {
    "err_cooldown": "10m",
    "err_threshold": 5,
    "hard_credit": "12h",
    "soft_rate": "60s"
  },
  "schedule": {
    "checkin_enabled": true,
    "checkin_hours": [21],
    "keepalive_hours": [19],
    "renew_enabled": true,
    "renew_hours": [20],
    "activity_hours": [22],
    "queue_enabled": true,
    "queue_hours": [10],
    "travel_hours": [0, 4, 8, 12, 16, 20],
    "cat_enabled": false,
    "school_enabled": false,
    "compensation_enabled": false,
    "random_delay_minutes": 10,
    "account_delay_max_seconds": 10
  }
}
```

- `api_key`、`upstash.token/url` 等敏感值只存在于 R28S 的 `/opt/wb2api/config/config.json`，不进入仓库、本文和截图。
- 当前配置了 Upstash 状态镜像；地址与令牌不入文档。
- `panel.enabled` 或 `admin.enabled` 为 true 时 `api_key` 不能为空，否则服务拒绝启动。

## 7. 日常启停与检查

```bash
# 容器
docker start workbuddy2api
docker stop workbuddy2api
docker restart workbuddy2api
docker logs --tail 100 -f workbuddy2api

# 健康 / 身份
wget -qO- http://127.0.0.1:7863/healthz
wget -qO- http://127.0.0.1:17863/healthz

# 进程与端口
ps | grep wb2api
netstat -tlnp | grep -E '7863|17863'
```

面板：`http://<R28S 的 LAN 或 Tailscale 地址>:7863/`，登录密码为 `config.json` 的 `api_key`。

## 8. 升级流程

1. **备份**（建议停容器做一致性快照）：
   ```bash
   docker stop workbuddy2api
   tar -czf /opt/wb2api-backup-$(date +%Y%m%d-%H%M).tar.gz \
     -C /opt/wb2api config auths data
   docker start workbuddy2api
   ```
2. 在开发机构建带版本号的 arm64 二进制（见 §4）。
3. 传到 R28S 的临时路径并替换：
   ```bash
   scp wb2api-low-write root@<R28S>:/opt/wb2api/bin/wb2api-low-write.new
   ssh root@<R28S> 'chmod 755 /opt/wb2api/bin/wb2api-low-write.new && \
     mv /opt/wb2api/bin/wb2api-low-write.new /opt/wb2api/bin/wb2api-low-write'
   ```
4. 重启容器并验收：
   ```bash
   ssh root@<R28S> 'docker restart workbuddy2api && sleep 3 && \
     wget -qO- http://127.0.0.1:7863/healthz'
   ```
5. 打开面板确认版本号、账号数与任务排程；首启日志应出现 `loaded N cn account(s)` 与 `listening on :7863`。

> 二进制是只读 bind mount：`docker cp` 进容器不会持久化，且替换宿主机文件后必须 `docker restart` 才会重新挂载。

## 9. 备份与恢复

- **备份内容**：`config/`（含 `api_key`）、`auths/`（账号凭证）、`data/`（`state.json`、`usage.json`、积分流水、密钥摘要）。
- **不要**把 `wb2api-image.tar`、旧二进制、`*.bak` 当作唯一备份；它们是构建/回滚残留，不是业务数据。
- 恢复：
  ```bash
  docker stop workbuddy2api
  tar -xzf /opt/wb2api-backup-YYYYMMDD-HHMM.tar.gz -C /opt/wb2api
  chown -R root:root /opt/wb2api/config /opt/wb2api/auths /opt/wb2api/data
  chmod 700 /opt/wb2api/auths /opt/wb2api/data
  chmod 600 /opt/wb2api/auths/*.json /opt/wb2api/data/*.json
  docker start workbuddy2api
  ```
- 恢复后检查 `/healthz` 的 `healthy/total`、面板账号数、最近签到/余额刷新时间；不要只看容器 `Up`。

## 10. SD 卡写入与容量

- 可写目录只有 `config/`、`auths/`、`data/`；实测 `data` 约 0.5 MiB（`usage.json` 约 0.5 MiB、`state.json` 约 10 KiB、积分流水约 36 KiB），`auths` 约 64 KiB。
- `usage.json` 有 30s 防抖原子写；`state.json` 原子写。**不要**把 `data/` 放到 tmpfs：掉电/重启会丢轮转状态、积分快照和密钥摘要。
- 容器日志目前是 json-file 默认策略，没有大小上限。重建容器时建议：
  ```bash
  --log-opt max-size=10m --log-opt max-file=3
  ```
  这是**建议项**，不是当前已生效配置。
- 不建议在 R28S 上跑前端构建（Node/Next.js 内存峰值会挤占 HA / AdGuard）。需要改前端时在开发机构建 `web/out`，再把产物同步回仓库并重新编译后端。

## 11. 安全边界

- WAN 侧默认 `REJECT`；7863 只应在 LAN / Tailscale 内可达。不要把 7863 直接暴露到公网；如必须公网访问，置于 HTTPS 反代之后并配置 `security.trusted_proxy_cidrs`。
- 17863 管理桥没有额外鉴权，**禁止公网放行**；只给本人管理设备使用。
- `api_key` 是面板和 API 的唯一凭证；不要写进仓库、聊天记录或截图。`auths/`、`data/`、`config/config.json` 已在 `.gitignore` 中排除。
- 外部/共享入口只放行 7863 时，仍需注意它同时承载面板与 `/admin/*`；不要在共享入口上使用弱 key，必要时用 `security.ip_whitelist` / Tailscale ACL 限定来源。
- 如需公网访问，务必加 HTTPS 反代；Tailscale 隧道加密不等于浏览器 HTTPS。

## 12. 已知差异与排障

| 现象 | 原因 | 处理 |
|---|---|---|
| 面板版本显示 `dev`，`/api/system/check-update` 的 `current` 也是 `dev` | 本地构建没有注入 `appVersion` | 按 §4 重新构建并注入 `-X workbuddy2api/internal/server.appVersion=vX.Y.Z`，替换二进制后重启 |
| 账号突然被禁用，`disabled_reason` 为 `upstream 401 gateway page (session revoked), re-login required` | 上游鉴权层返回 401 HTML（会话被吊销，常见于账号在别处重新登录） | v1.19.3 起六条链路自动禁用；在面板重新扫码/授权登录该账号 |
| 容器启动后账号数为 0 | `auths/` 挂载路径或权限不对 | 检查 `auth_dir` 与挂载；`auths` 目录需容器用户可读，文件 0600 |
| 面板热改配置返回 200 但重启回滚 | `config` 挂成了单文件，`tmp+rename` 被 EBUSY | 改成目录挂载 `/opt/wb2api/config:/app/config` |
| DNS 解析失败 / 上游超时 | 容器继承了 OpenWrt 上不可用的 DNS | 检查 `/opt/wb2api/resolv.conf`，保留本机 DNS + 公共 DNS，重启容器 |
| `docker logs` 占满 SD 卡 | json-file 无上限 | 重建容器时加 `--log-opt max-size=10m --log-opt max-file=3` |

## 13. 验收清单

- [ ] `docker ps` 显示 `workbuddy2api` 为 `Up`，restart policy 为 `unless-stopped`
- [ ] `wget -qO- http://127.0.0.1:7863/healthz` 返回 healthy/total 且无异常
- [ ] `wget -qO- http://127.0.0.1:17863/healthz` 同样返回 healthy
- [ ] 面板可登录，账号数与 `auths/*.json` 数量一致
- [ ] `data/usage.json`、`state.json` 在请求后更新时间前进
- [ ] `docker logs` 无持续 401 / 12153 / panic
- [ ] 备份 tar 包可列出 `config/auths/data` 且大小合理
