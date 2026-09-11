# WorkBuddy Manager

为 [`workbuddy2api`](https://github.com/Sliverkiss/workbuddy2api)（腾讯 CodeBuddy → OpenAI 兼容代理，Go）打造的 **Web 管理控制台 + 对外反代网关**。

- **管理端**：多账号可视化、扫码纳管、自动/手动签到、Token 有效期监控、连通性测试、模型别名映射、上游配置可视化。
- **反代网关**：对外提供 `/v1/chat/completions`，支持多用户密钥、入站 IP 白/黑名单、每密钥 IP 上限与白名单、模型白名单、配额、请求日志与 Token 用量统计。
- **UI**：视觉与**浮动底栏**交互 1:1 复刻 [`linux-do/cdk`](https://github.com/linux-do/cdk)（LDC）—— shadcn/ui (new-york) + Tailwind v4 + zinc 中性灰，亮色为主 / 暗色可切。

> 本方案**不修改** workbuddy2api 的 Go 代码；账号轮询与并发仍由其负责。

---

## 架构

```
客户端 / sub2api（OpenAI SDK）
   │  Authorization: Bearer wbk_xxx
   ▼
[WorkBuddy Manager :7864]        ← 1Panel 反代 + HTTPS
   ├─ Next.js 15 静态导出（shadcn/ui + LDC 浮动底栏）
   ├─ FastAPI  /api/*   管理接口（登录 / 账号 / 密钥 / 日志 / 用量 / 安全 / 设置）
   └─ FastAPI  /v1/*  /v2/*  /healthz   对外网关
        密钥鉴权 → IP 管控 → 模型映射 → 流式转发 → 日志与用量落库(SQLite)
   ▼
[workbuddy2api :7863]（不改动）→ copilot.tencent.com
```

生产为单进程单端口：前端由 `next build` 静态导出到 `web/out`，交给 FastAPI 托管，`/api`、`/v1` 同源，无 CORS。

---

## 目录结构

```
workbuddy-manager/
├─ server/                  # FastAPI 后端
│  ├─ main.py               # 入口：路由 + 静态托管
│  ├─ config.py             # 全部环境变量
│  ├─ db.py                 # SQLite（密钥/日志/用量/IP 规则/设置）
│  ├─ security.py           # PBKDF2 + 签名 Cookie 会话 + 防爆破
│  ├─ keysvc.py             # 密钥生成/校验/限额
│  ├─ iputil.py             # 真实 IP 解析 + CIDR 匹配
│  ├─ services/
│  │  ├─ tencent.py         # 腾讯登录/签到/探测协议
│  │  └─ wb2api.py          # workbuddy2api 交互（账号文件/状态/模型/重启）
│  └─ routers/              # auth accounts keys logs stats security settings gateway
├─ web/                     # Next.js 15 前端（fork 自 LDC 的组件层）
│  ├─ app/(main)/           # dashboard accounts keys logs stats security settings
│  ├─ components/ui/        # shadcn 原语（含 floating-dock.tsx，来自 LDC）
│  └─ components/common/    # ManagementBar（浮动底栏）、StatCard、各业务组件
└─ deploy/                  # systemd unit + 一键部署脚本
```

---

## 本地开发

```bash
# 1) 后端（终端 A）
python -m pip install -r server/requirements.txt
WB_ADMIN_PASSWORD=admin123 \
WB_DATA_DIR=./data \
WB_AUTH_DIR=/opt/workbuddy2api/auths \
WB2API_BASE=http://127.0.0.1:7863 \
python -m uvicorn server.main:app --reload --port 7864

# 2) 前端（终端 B）—— next dev 会把 /api、/v1 反代到 :7864
cd web
npm install
npm run dev            # http://localhost:3000
```

首次启动会在 `WB_DATA_DIR` 生成 `users.json` 与随机签名密钥；若未设 `WB_ADMIN_PASSWORD`，会打印一次随机管理员密码。

---

## 生产部署（Ubuntu + 1Panel）

```bash
# 本地先构建静态前端
cd web && npm install && npm run build:export     # 产出 web/out

# 上传仓库到服务器后
sudo bash deploy/install.sh                        # 安装依赖 + 注册 systemd
journalctl -u workbuddy-web -f                     # 查看初始管理员密码
```

**1Panel 反代**：网站 → 创建反向代理 → 域名 `wb.sbai.shop` → 目标 `http://127.0.0.1:7864` → 申请 Let's Encrypt 证书并开启强制 HTTPS。

### 环境变量

| 变量 | 默认 | 说明 |
|---|---|---|
| `WB_MANAGER_PORT` | `7864` | 监听端口 |
| `WB2API_BASE` | `http://127.0.0.1:7863` | workbuddy2api 地址 |
| `WB2API_KEY` | 读 config.json | 上游 API Key |
| `WB2API_CONTAINER` | `workbuddy2api` | 重启用的容器名 |
| `WB_AUTH_DIR` | `/opt/workbuddy2api/auths` | 账号授权目录 |
| `WB_DATA_DIR` | `./data` | 本服务数据目录 |
| `WB_STATIC_DIR` | `./web/out` | 静态导出目录 |
| `WB_ADMIN_PASSWORD` | 随机 | 首次启动的 admin 密码 |
| `WB_SECURE_COOKIE` | `auto` | 依 `X-Forwarded-Proto` 判定 |

完整清单见 [.env.example](.env.example)。

---

## 使用

1. 登录后在「账号」页点击 **添加账号**，用微信 / QQ 扫码；成功后自动签到、落盘并重启上游容器。
2. 在「密钥」页创建 `wbk_...`（仅创建时展示一次），可设有效期、IP 白名单、每密钥最大 IP 数、模型白名单与配额。
3. 下游按 OpenAI 协议接入：

```bash
curl https://wb.sbai.shop/v1/chat/completions \
  -H "Authorization: Bearer wbk_xxxxxxxx" \
  -H "Content-Type: application/json" \
  -d '{"model":"glm-5.2","messages":[{"role":"user","content":"你好"}],"stream":true}'
```

- Base URL：`https://<你的域名>/v1`
- 模型：`glm-5.2` / `glm-5.1` / `glm-5v-turbo` / `kimi-k2.7` / `minimax-m3` / `hy3` 等，可在「设置 → 模型映射」配置别名。
- 流式请求会自动注入 `stream_options.include_usage=true`，以便精确计量 Token。

---

## 安全说明

- `users.json`、`data/*.db`、`.env` 已在 `.gitignore` 中排除，**请勿提交**。
- 公网暴露务必走 HTTPS（Cookie 与密码保护）。
- 密钥仅存 SHA-256 哈希，明文只返回一次。
- 登录接口对同 IP 连续失败 5 次锁定 10 分钟；可叠加 1Panel IP 白名单 / Cloudflare Access。

---

## 已知限制 / 路线图

- **出站 IP 隔离（二期）**：当前网关只做**入站** IP 管控。若要像 AGM 那样为每个腾讯账号绑定独立**出口代理/IP**，需在 workbuddy2api 的 Go 侧增加代理池支持（上游请求由其发出），本仓库不包含该改动。
- 请求的**请求体/响应体内容**不做留存，仅记录元数据（模型、状态、Token、延迟）以保护隐私。
- 用量统计按「天 × 密钥 × 模型」聚合；如需按小时粒度可扩展 `usage_daily`。

---

## 致谢

- [`linux-do/cdk`](https://github.com/linux-do/cdk)（MIT）—— UI 设计令牌与浮动底栏组件来源。
- [`Sliverkiss/workbuddy2api`](https://github.com/Sliverkiss/workbuddy2api) —— 底层账号池与 OpenAI 兼容代理。
- [`lbjlaq/Antigravity-Manager`](https://github.com/lbjlaq/Antigravity-Manager) —— 管理端功能形态参考。

## License

MIT
