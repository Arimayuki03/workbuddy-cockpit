# Windows 原生部署：上游启停脚本模板

管理端支持上游以**本机进程**（而非 Docker 容器）运行时，需要一对启停脚本 ——
在 `.env` 里这样配置：

```dotenv
WB2API_MODE=native
WB2API_START_SCRIPT=C:/path/to/workbuddy2api/start-workbuddy2api.cmd
WB2API_STOP_SCRIPT=C:/path/to/workbuddy2api/stop-workbuddy2api.cmd
WB2API_LOG_FILE=C:/path/to/workbuddy2api/data/server.err.log
```

**为什么这里放着模板、而不是上游自带**：上游 `workbuddy2api` 官方只提供 Docker
部署（其 README 只有 `docker compose up -d --build` 一种方式），没有 Windows 原生
的启停脚本。所以这个目录提供一对可直接改用的模板 —— 把两个 `.cmd` 复制到你的上游
目录，按里面的 `TODO` 改路径即可。

## 两个必须遵守的约定

**1. 启动脚本必须立即返回。**

管理端是「调用脚本 → 等它结束 → 认为重启完成」。如果脚本在前台一直运行（例如
直接 `wb2api.exe ...` 而不加 `start /b`），管理端会一直等到超时（60 秒）才失败 ——
上游其实已经起来了，但界面报的是失败。

模板里用 `start "workbuddy2api" /b ...` 解决：后台拉起并立即返回。

**2. 日志要写到 `WB2API_LOG_FILE` 指向的文件。**

管理端「任务记录」页的自动任务日志（旅行领奖、签到、保活…）是从那个文件读的，
不是从 `docker logs`。模板里把 stdout/stderr 分别重定向到 `server.out.log` /
`server.err.log`，所以 `.env` 的 `WB2API_LOG_FILE` 要指向 `server.err.log`。

## 上游可执行文件从哪来

上游用 Go 写，需要自己构建（同样是因为它只发布 Docker 镜像）：

```powershell
cd C:\path\to\workbuddy2api
go build -o wb2api.exe ./cmd/server
```

构建后目录里应有 `wb2api.exe`、`config.json`、`auths\`、`data\`。
登录账号可以继续用上游自带的 `login.sh`（需要 Git Bash / WSL），
或在容器里登录后把 `auths\` 拷出来 —— 总之管理端只要求 `auths\` 里有
`workbuddy-*.json`。

## 能力边界

原生模式下**网页一键更新不可用**（它依赖 Linux/Docker 完成代码替换与服务重启）。
在界面上点会得到明确提示，不会执行到一半才失败。更新上游请手动拉代码、
重新 `go build`，然后重启管理端。
